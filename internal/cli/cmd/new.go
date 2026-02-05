package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gelotto/hqsshd/internal/cli/client"
	pb "github.com/gelotto/hqsshd/proto"
	"github.com/spf13/cobra"
)

var (
	newTool     string
	newProject  string
	newNoAttach bool
)

var newCmd = &cobra.Command{
	Use:   "new",
	Short: "Create a new session",
	Long: `Create a new AI session on the remote system.

By default, creates a shell session and attaches to it immediately.
Use --tool to specify which AI tool to launch.

Examples:
  hqssh new                              # New shell session
  hqssh new --tool claude                # New Claude session
  hqssh new --tool claude --project .    # In current directory
  hqssh new --tool aider --no-attach     # Create but don't attach`,
	RunE: runNew,
}

func init() {
	newCmd.Flags().StringVarP(&newTool, "tool", "t", "shell", "Tool to launch: claude, codex, aider, shell")
	newCmd.Flags().StringVarP(&newProject, "project", "d", "", "Working directory or project path")
	newCmd.Flags().BoolVar(&newNoAttach, "no-attach", false, "Create session but don't attach")
	rootCmd.AddCommand(newCmd)
}

func runNew(cmd *cobra.Command, args []string) error {
	cfg := resolveConfig()

	if cfg.Host == "" {
		return fmt.Errorf("no host specified\n\nProvide a host using one of:\n  --host/-H flag:    hqssh new -H server.example.com\n  Environment var:   export HQSSH_HOST=server.example.com\n  Config file:       ~/.hqssh/config.yaml with default_host set")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle Ctrl-C gracefully
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)
	go func() {
		<-sigChan
		fmt.Fprintf(os.Stderr, "\nDetaching...\n")
		cancel()
	}()

	// Connect to daemon
	fmt.Fprintf(os.Stderr, "Connecting to %s...\n", cfg.Host)
	c, err := client.ConnectWithRetry(ctx, client.Config{
		Host:            cfg.Host,
		Port:            cfg.Port,
		User:            cfg.User,
		KeyPath:         cfg.Key,
		Password:        cfg.Password,
		InsecureHostKey: insecureKey,
	})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer c.Close()

	// Get terminal size
	cols, rows := termSize()

	// Pass --project value as-is to the daemon.
	// The daemon resolves relative paths on its own filesystem.
	// Do NOT resolve locally — "." means the remote CWD, not the local one.
	workingDir := newProject

	// Create the session
	fmt.Fprintf(os.Stderr, "Creating %s session...\n", newTool)
	session, err := c.SessionService.Create(ctx, &pb.CreateSessionRequest{
		Tool:             newTool,
		WorkingDirectory: workingDir,
		Cols:             int32(cols),
		Rows:             int32(rows),
	})
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Session created: %s\n", shortID(session.Id))

	if newNoAttach {
		fmt.Fprintf(os.Stderr, "\nTo attach later:\n")
		fmt.Fprintf(os.Stderr, "  hqssh attach %s -H %s\n", shortID(session.Id), cfg.Host)
		return nil
	}

	// Attach to the session (reuse shared attach logic)
	fmt.Fprintf(os.Stderr, "Attaching to session %s...\n", shortID(session.Id))

	return attachToSession(ctx, cancel, c, session.Id, cfg.Host)
}
