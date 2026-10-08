// Copyright 2024 Gelotto
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package shellutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseInteractivePATH(t *testing.T) {
	cases := []struct {
		out    string
		want   string
		wantOK bool
	}{
		{"\n" + interactivePathMarker + "/a:/b\n", "/a:/b", true},
		{"Welcome!\nnvm: using v22\n\n" + interactivePathMarker + "/a\r\n", "/a", true},
		{interactivePathMarker + "/old\n" + interactivePathMarker + "/new\n", "/new", true},
		{"no marker here\n", "", false},
		{"\n" + interactivePathMarker + "\n", "", false},
	}
	for _, tc := range cases {
		got, ok := parseInteractivePATH(tc.out)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("parseInteractivePATH(%q) = %q, %v; want %q, %v", tc.out, got, ok, tc.want, tc.wantOK)
		}
	}
}

// PATH entries added only by interactive init (~/.zshrc; ~/.bashrc behind the
// stock non-interactive guard) must be returned, and init that prints or
// prompts for input must neither leak into the result nor block.
func TestInteractivePATH_ReadsInteractiveInit(t *testing.T) {
	cases := []struct {
		shell string
		files func(binDir string) map[string]string
	}{
		{"/bin/zsh", func(binDir string) map[string]string {
			return map[string]string{
				".zshrc": "echo 'Welcome banner'\n" +
					"read -q 'ans?Update now? [y/N] '\n" +
					"export PATH=\"" + binDir + ":$PATH\"\n",
			}
		}},
		{"/bin/bash", func(binDir string) map[string]string {
			return map[string]string{
				".bash_profile": "[ -f ~/.bashrc ] && . ~/.bashrc\n",
				".bashrc": "case $- in *i*) ;; *) return;; esac\n" +
					"echo 'Welcome banner'\n" +
					"read -r -p 'Update now? [y/N] ' ans\n" +
					"export PATH=\"" + binDir + ":$PATH\"\n",
			}
		}},
	}
	for _, tc := range cases {
		t.Run(filepath.Base(tc.shell), func(t *testing.T) {
			if _, err := os.Stat(tc.shell); err != nil {
				t.Skipf("%s not installed", tc.shell)
			}
			home := t.TempDir()
			binDir := filepath.Join(home, "nvm-bin")
			for name, body := range tc.files(binDir) {
				if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", home)
			t.Setenv("ZDOTDIR", home)
			t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
			t.Setenv("SHELL", tc.shell)

			start := time.Now()
			path, ok := InteractivePATH(5 * time.Second)
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Errorf("probe took %v: prompting init blocked", elapsed)
			}
			if !ok {
				t.Fatal("InteractivePATH() ok = false")
			}
			if !strings.Contains(path, binDir) {
				t.Errorf("PATH %q misses %s (set only in interactive init)", path, binDir)
			}
			if strings.Contains(path, "Welcome") {
				t.Errorf("init output leaked into PATH: %q", path)
			}
		})
	}
}
