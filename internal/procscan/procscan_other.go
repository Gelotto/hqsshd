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

//go:build !linux && !darwin

package procscan

import "time"

import "fmt"

// External session discovery is only implemented for Linux (/proc) and
// macOS (sysctl + lsof). Elsewhere Scan finds nothing and Kill refuses.

func listProcesses() map[int]procInfo { return nil }

func readArgv(pid int) ([]string, error) {
	return nil, fmt.Errorf("external session discovery unsupported on this platform")
}

func cwdFor(pids []int) map[int]string { return nil }

// LsofPath is unavailable on this platform.
func LsofPath() string { return "" }

// BootTime is unavailable on this platform.
func BootTime() (time.Time, bool) { return time.Time{}, false }
