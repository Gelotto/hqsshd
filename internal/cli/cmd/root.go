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

package cmd

import (
	"fmt"
	"os"

	"github.com/gelotto/hqsshd/internal/cli/config"
	daemonconfig "github.com/gelotto/hqsshd/internal/config"
	"github.com/spf13/cobra"
)

var (
	// Global flags (raw CLI values)
	host        string
	port        int
	user        string
	keyPath     string
	password    string
	insecureKey bool
	socket      string
)

var rootCmd = &cobra.Command{
	Use:   "hqssh",
	Short: "HQSSH - Mobile-first SSH with AI session management",
	Long: `HQSSH CLI - Attach to AI sessions started from your mobile device.

LOCAL DAEMON (zero-config):
  If hqsshd is running locally, just run commands directly:

  hqssh sessions                     # Auto-detects local daemon
  hqssh attach abc12345              # Attach to a local session
  hqssh new --tool claude            # Create a new session

REMOTE HOST:
  hqssh sessions -H server.example.com
  hqssh attach abc12345 -H server.example.com
  hqssh new --tool claude -H server.example.com

EXPLICIT SOCKET:
  hqssh sessions -S /path/to/hqssh.sock

CONFIG:
  Create ~/.hqssh/config.yaml to set defaults and avoid typing -H every time:

    default_host: myserver
    hosts:
      myserver:
        host: server.example.com
        user: ubuntu
        key: ~/.ssh/id_ed25519

  Then just run: hqssh sessions

  Environment variables also work (override config file):
    export HQSSH_HOST=server.example.com
    export HQSSH_USER=ubuntu
    hqssh sessions

CONNECTION PRIORITY:
  -S/--socket flag > -H/--host flag > config/env > local socket auto-detect`,
}

func init() {
	// Global flags
	rootCmd.PersistentFlags().StringVarP(&host, "host", "H", "", "Remote host or config alias")
	rootCmd.PersistentFlags().IntVarP(&port, "port", "P", 22, "SSH port")
	rootCmd.PersistentFlags().StringVarP(&user, "user", "u", os.Getenv("USER"), "SSH username")
	rootCmd.PersistentFlags().StringVarP(&keyPath, "key", "k", "", "SSH private key path")
	rootCmd.PersistentFlags().StringVarP(&password, "password", "p", "", "SSH password (not recommended)")
	rootCmd.PersistentFlags().BoolVar(&insecureKey, "insecure", false, "Skip host key verification")
	rootCmd.PersistentFlags().StringVarP(&socket, "socket", "S", "", "Unix socket path (local daemon)")

	// Add subcommands
	rootCmd.AddCommand(sessionsCmd)
	rootCmd.AddCommand(attachCmd)
	rootCmd.AddCommand(versionCmd)
}

func Execute() error {
	return rootCmd.Execute()
}

// resolveConfig returns the resolved connection settings from all sources.
func resolveConfig() config.Resolved {
	// Check if port was explicitly changed from default
	cliPort := 0
	if rootCmd.PersistentFlags().Changed("port") {
		cliPort = port
	}

	// Check if user was explicitly set
	cliUser := ""
	if rootCmd.PersistentFlags().Changed("user") {
		cliUser = user
	}

	return config.Resolve(host, cliUser, keyPath, password, cliPort)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("hqssh CLI v%s\n", daemonconfig.DaemonVersion)
	},
}
