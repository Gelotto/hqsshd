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

package servicemgr

import (
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/gelotto/hqsshd/internal/config"
)

// DataDir returns ~/.hqssh (config owns the location).
func DataDir() (string, error) {
	return config.DataDir()
}

// DefaultLogFile is where the installer points launchd's StandardOutPath /
// StandardErrorPath. On Linux the daemon logs to journald instead.
func DefaultLogFile() string {
	dir, err := DataDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "logs", "hqsshd.log")
}

// MaxSocketPathLen is the longest Unix socket path this OS accepts: the
// kernel's sun_path minus the terminating NUL — 103 bytes on macOS, 107 on
// Linux. Longer paths fail at bind with the unhelpful "invalid argument".
func MaxSocketPathLen() int {
	return len(unix.RawSockaddrUnix{}.Path) - 1
}
