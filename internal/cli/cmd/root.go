package cmd

import (
	"fmt"
	"os"

	"github.com/gelotto/hqsshd/internal/cli/config"
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

	// portChanged tracks if -P was explicitly set
	portChanged bool
)

var rootCmd = &cobra.Command{
	Use:   "hqssh",
	Short: "HQSSH - Mobile-first SSH with AI session management",
	Long: `HQSSH CLI - Attach to AI sessions started from your mobile device.

QUICK START:
  # List sessions on a host
  hqssh sessions -H server.example.com

  # Attach to a session
  hqssh attach abc12345 -H server.example.com

  # Create new session
  hqssh new --tool claude -H server.example.com

  # Kill a session
  hqssh kill abc12345 -H server.example.com

  # View system info
  hqssh info -H server.example.com

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

PRIORITY:
  CLI flags > Environment variables > Config file > Defaults`,
}

func init() {
	// Global flags
	rootCmd.PersistentFlags().StringVarP(&host, "host", "H", "", "Remote host or config alias")
	rootCmd.PersistentFlags().IntVarP(&port, "port", "P", 22, "SSH port")
	rootCmd.PersistentFlags().StringVarP(&user, "user", "u", os.Getenv("USER"), "SSH username")
	rootCmd.PersistentFlags().StringVarP(&keyPath, "key", "k", "", "SSH private key path")
	rootCmd.PersistentFlags().StringVarP(&password, "password", "p", "", "SSH password (not recommended)")
	rootCmd.PersistentFlags().BoolVar(&insecureKey, "insecure", false, "Skip host key verification")

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
		fmt.Println("hqssh CLI v0.1.0")
	},
}
