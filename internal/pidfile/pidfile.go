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

// Package pidfile records the running daemon's pid in ~/.hqssh/hqsshd.pid
// and guards it with an exclusive flock(2) held for the daemon's lifetime.
//
// The lock, not the pid, is the liveness signal: it is released by the
// kernel when the process dies, so it survives neither a SIGKILL nor a
// reboot, and a pid recycled to an unrelated process can never impersonate
// a running daemon. The pid inside the file is informational — it names
// the holder in "already running" messages and in `hqssh doctor`.
package pidfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/gelotto/hqsshd/internal/fsutil"
)

// FileName is the pidfile's name inside the daemon data directory.
const FileName = "hqsshd.pid"

var (
	// ErrLocked reports that another process holds the pidfile.
	ErrLocked = errors.New("pidfile is locked by a running daemon")
	// ErrCorrupt reports a pidfile whose content is not a pid.
	ErrCorrupt = errors.New("pidfile does not contain a pid")
)

// Status classifies a pidfile.
type Status int

const (
	// StatusNone means no pidfile exists.
	StatusNone Status = iota
	// StatusStale means the file exists but nobody holds its lock: its
	// owner died without cleaning up.
	StatusStale
	// StatusAlive means a process holds the lock.
	StatusAlive
)

func (s Status) String() string {
	switch s {
	case StatusStale:
		return "stale"
	case StatusAlive:
		return "alive"
	default:
		return "none"
	}
}

// Path returns the pidfile path inside dataDir.
func Path(dataDir string) string {
	return filepath.Join(dataDir, FileName)
}

// Lock is a held pidfile.
type Lock struct {
	f    *os.File
	path string
}

// Acquire opens (creating if needed) the pidfile, takes its exclusive lock
// without blocking, and records pid in it. When another process holds the
// lock it returns ErrLocked together with that process's pid (0 when the
// file could not be read).
func Acquire(path string, pid int) (*Lock, int, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, 0, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, 0, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := readPid(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, holder, ErrLocked
		}
		return nil, 0, fmt.Errorf("lock %s: %w", path, err)
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, 0, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(pid)+"\n"), 0); err != nil {
		f.Close()
		return nil, 0, err
	}
	f.Sync() //nolint:errcheck // best effort; the lock is what matters
	return &Lock{f: f, path: path}, 0, nil
}

// Release removes the pidfile and drops the lock. The file is only removed
// while it is still the locked one — a newer file renamed over the path by
// someone else is left alone. Safe to call more than once.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	locked, err1 := l.f.Stat()
	onDisk, err2 := os.Stat(l.path)
	if err1 == nil && err2 == nil && os.SameFile(locked, onDisk) {
		os.Remove(l.path)
	}
	err := l.f.Close() // releases the flock
	l.f = nil
	return err
}

// Path returns the locked file's path.
func (l *Lock) Path() string { return l.path }

// Check reports whether a daemon holds the pidfile by trying its lock
// without blocking. No process inspection is involved, so a recycled pid
// cannot produce a false "alive".
func Check(path string) (Status, int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return StatusNone, 0, nil
		}
		return StatusNone, 0, err
	}
	defer f.Close()

	pid, perr := readPid(f)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return StatusAlive, pid, nil
		}
		return StatusNone, 0, fmt.Errorf("lock %s: %w", path, err)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return StatusStale, pid, perr
}

// Read returns the pid recorded at path. A missing file yields an error
// that satisfies os.IsNotExist; unparsable content yields ErrCorrupt.
func Read(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return readPid(f)
}

// Write records pid at path atomically without locking it (used by tests
// and tools that emulate a foreign daemon).
func Write(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, []byte(strconv.Itoa(pid)+"\n"), 0600)
}

func readPid(f *os.File) (int, error) {
	buf := make([]byte, 32)
	n, err := f.ReadAt(buf, 0)
	if err != nil && n == 0 {
		return 0, fmt.Errorf("%w: empty", ErrCorrupt)
	}
	text := strings.TrimSpace(string(buf[:n]))
	pid, err := strconv.Atoi(text)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("%w: %q", ErrCorrupt, text)
	}
	return pid, nil
}
