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

// Package fsutil holds small filesystem helpers shared by the daemon's
// persistent stores.
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteFileAtomic writes data to path via a uniquely named temporary file
// in the same directory ("<base>.<random>.tmp"), fsyncs it, applies perm,
// and renames it over path. Readers never observe a partial file, and the
// temporary file is removed on every failure path — a daemon killed
// mid-write (launchd's ExitTimeOut SIGKILL, a crash) leaves at most an
// orphan that RemoveStaleTemps sweeps on the next load.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	f, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()

	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err = f.Chmod(perm); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// StaleTemps lists leftovers from interrupted WriteFileAtomic calls next
// to path: "<base>.*.tmp" and the legacy fixed-name "<base>.tmp" that older
// daemons wrote.
func StaleTemps(path string) []string {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var stale []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		if name != base+".tmp" && !strings.HasPrefix(name, base+".") {
			continue
		}
		stale = append(stale, name)
	}
	return stale
}

// RemoveStaleTemps deletes what StaleTemps lists and returns the names
// removed so callers can log them. Errors are ignored: a stale temp file is
// cosmetic, never load-bearing.
func RemoveStaleTemps(path string) []string {
	dir := filepath.Dir(path)
	var removed []string
	for _, name := range StaleTemps(path) {
		if err := os.Remove(filepath.Join(dir, name)); err == nil {
			removed = append(removed, name)
		}
	}
	return removed
}
