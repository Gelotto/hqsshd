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

package pidfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireReleaseRoundTrip(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "data"))

	lock, holder, err := Acquire(path, 4242)
	if err != nil || holder != 0 || lock == nil {
		t.Fatalf("Acquire = %v, %d, %v", lock, holder, err)
	}
	if pid, err := Read(path); err != nil || pid != 4242 {
		t.Errorf("Read = %d, %v; want 4242", pid, err)
	}
	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("perm = %04o, want 0600", perm)
	}
	if status, pid, err := Check(path); err != nil || status != StatusAlive || pid != 4242 {
		t.Errorf("Check(held) = %v, %d, %v; want Alive 4242", status, pid, err)
	}

	// A second acquirer (another fd, same process — flock is per open file)
	// is refused and told who holds it.
	if _, holder, err := Acquire(path, 1); !errors.Is(err, ErrLocked) || holder != 4242 {
		t.Errorf("second Acquire = holder %d, %v; want ErrLocked by 4242", holder, err)
	}

	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("pidfile still exists after Release")
	}
	if err := lock.Release(); err != nil {
		t.Errorf("second Release = %v", err)
	}
}

func TestCheckStaleAndNone(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir)

	if status, pid, err := Check(path); err != nil || status != StatusNone || pid != 0 {
		t.Errorf("Check(none) = %v, %d, %v", status, pid, err)
	}

	// A file nobody locks: its writer died without cleaning up
	if err := Write(path, 777); err != nil {
		t.Fatal(err)
	}
	if status, pid, err := Check(path); err != nil || status != StatusStale || pid != 777 {
		t.Errorf("Check(stale) = %v, %d, %v; want Stale 777", status, pid, err)
	}

	// A stale file is taken over by the next acquirer
	lock, holder, err := Acquire(path, 778)
	if err != nil || holder != 0 {
		t.Fatalf("Acquire over stale = %d, %v", holder, err)
	}
	defer lock.Release()
	if pid, _ := Read(path); pid != 778 {
		t.Errorf("pid after takeover = %d, want 778", pid)
	}
}

func TestReadCorrupt(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad")
	os.WriteFile(bad, []byte("not a pid\n"), 0600)
	if _, err := Read(bad); !errors.Is(err, ErrCorrupt) {
		t.Errorf("corrupt Read error = %v, want ErrCorrupt", err)
	}
	status, pid, err := Check(bad)
	if status != StatusStale || pid != 0 || !errors.Is(err, ErrCorrupt) {
		t.Errorf("Check(corrupt) = %v, %d, %v; want Stale, 0, ErrCorrupt", status, pid, err)
	}
	// Acquire overwrites garbage
	lock, _, err := Acquire(bad, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if pid, err := Read(bad); err != nil || pid != 5 {
		t.Errorf("after Acquire over corrupt: %d, %v", pid, err)
	}
}

func TestReleaseKeepsAReplacementsFile(t *testing.T) {
	path := Path(t.TempDir())
	lock, _, err := Acquire(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Someone renamed a new file over the path (a different inode)
	if err := Write(path, 2); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if pid, err := Read(path); err != nil || pid != 2 {
		t.Errorf("replacement pidfile = %d, %v; want 2 kept", pid, err)
	}
}
