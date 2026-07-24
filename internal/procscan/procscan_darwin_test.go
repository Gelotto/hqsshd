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

//go:build darwin

package procscan

import (
	"encoding/binary"
	"os"
	"reflect"
	"testing"
)

// procArgs2Buf builds a synthetic KERN_PROCARGS2 buffer.
func procArgs2Buf(execPath string, argv []string, env []string) []byte {
	var buf []byte
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(argv)))
	buf = append(buf, execPath...)
	buf = append(buf, 0, 0, 0) // Terminator plus alignment padding
	for _, a := range argv {
		buf = append(buf, a...)
		buf = append(buf, 0)
	}
	for _, e := range env {
		buf = append(buf, e...)
		buf = append(buf, 0)
	}
	return buf
}

func TestParseProcArgs2(t *testing.T) {
	buf := procArgs2Buf("/usr/local/bin/node",
		[]string{"node", "/usr/local/bin/claude", "--continue"},
		[]string{"HOME=/Users/u", "TERM=xterm"})

	argv, err := parseProcArgs2(buf)
	if err != nil {
		t.Fatalf("parseProcArgs2: %v", err)
	}
	want := []string{"node", "/usr/local/bin/claude", "--continue"}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %v, want %v", argv, want)
	}

	// The environment must not leak into argv when argc bounds the read
	for _, a := range argv {
		if a == "HOME=/Users/u" {
			t.Error("environment leaked into argv")
		}
	}

	if _, err := parseProcArgs2([]byte{1, 0}); err == nil {
		t.Error("expected error for short buffer")
	}
	if _, err := parseProcArgs2(binary.LittleEndian.AppendUint32(nil, 0)); err == nil {
		t.Error("expected error for zero argc")
	}
}

func TestReadArgv_Self(t *testing.T) {
	argv, err := readArgv(os.Getpid())
	if err != nil {
		t.Fatalf("readArgv(self): %v", err)
	}
	if len(argv) == 0 {
		t.Fatal("empty argv for own process")
	}
}

func TestParseLsofCwd(t *testing.T) {
	out := []byte("p123\nfcwd\nn/Users/u/proj\np456\nfcwd\nn/tmp/other dir\n")
	got := parseLsofCwd(out)
	want := map[int]string{123: "/Users/u/proj", 456: "/tmp/other dir"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseLsofCwd = %v, want %v", got, want)
	}

	if got := parseLsofCwd(nil); len(got) != 0 {
		t.Errorf("empty output should yield no cwds, got %v", got)
	}
}
