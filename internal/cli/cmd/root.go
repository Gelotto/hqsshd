package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	// Global flags
	host        string
	port        int
	user        string
	keyPath     string
	password    string
	insecureKey bool
)

var rootCmd = &cobra.Command{
	Use:   "hqssh",
	Short: "HQSSH - Mobile-first SSH with AI session management",
	Long: `HQSSH CLI - Attach to AI sessions started from your mobile device.

Use this tool to:
  - List active sessions on a remote system
  - Attach to an AI session (Claude, Codex, Aider) for mobile-to-desktop handoff
  - Continue working on the same session from your desktop`,
}

func init() {
	// Global flags
	rootCmd.PersistentFlags().StringVarP(&host, "host", "H", "", "Remote host (required)")
	rootCmd.PersistentFlags().IntVarP(&port, "port", "P", 22, "SSH port")
	rootCmd.PersistentFlags().StringVarP(&user, "user", "u", os.Getenv("USER"), "SSH username")
	rootCmd.PersistentFlags().StringVarP(&keyPath, "key", "k", "", "SSH private key path")
	rootCmd.PersistentFlags().StringVarP(&password, "password", "p", "", "SSH password (not recommended, use key)")
	rootCmd.PersistentFlags().BoolVar(&insecureKey, "insecure", false, "Skip host key verification (not recommended)")

	// Add subcommands
	rootCmd.AddCommand(sessionsCmd)
	rootCmd.AddCommand(attachCmd)
	rootCmd.AddCommand(versionCmd)
}

func Execute() error {
	return rootCmd.Execute()
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("hqssh CLI v0.1.0")
	},
}
