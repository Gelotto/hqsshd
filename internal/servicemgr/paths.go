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

// DefaultLogFile is where the installer points a launchd user agent's
// StandardOutPath / StandardErrorPath. On Linux the daemon logs to journald
// instead.
func DefaultLogFile() string {
	dir, err := DataDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "logs", "hqsshd.log")
}

// SystemLogFile is the launchd system daemon's log. launchd opens the
// StandardOutPath as root, so it must not live under the user's HOME
// (a user-plantable symlink there would be followed by root); the
// installer creates the directory root-owned.
const SystemLogFile = "/Library/Logs/hqsshd/hqsshd.log"

// LogFile returns the launchd log location for a service scope.
func LogFile(scope Scope) string {
	if scope == ScopeSystem {
		return SystemLogFile
	}
	return DefaultLogFile()
}

// LogFileFor is LogFile for an optional service (nil = user default).
func LogFileFor(svc *Service) string {
	if svc == nil {
		return DefaultLogFile()
	}
	return LogFile(svc.Scope)
}

// MaxSocketPathLen is the longest Unix socket path this OS accepts: the
// kernel's sun_path minus the terminating NUL — 103 bytes on macOS, 107 on
// Linux. Longer paths fail at bind with the unhelpful "invalid argument".
func MaxSocketPathLen() int {
	return len(unix.RawSockaddrUnix{}.Path) - 1
}
