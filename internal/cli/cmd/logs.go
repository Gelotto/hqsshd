package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gelotto/hqsshd/internal/cli/client"
	pb "github.com/gelotto/hqsshd/proto"
	"github.com/spf13/cobra"
)

var logsCmd = &cobra.Command{
	Use:   "logs <session-id>",
	Short: "View session log (works for active and ended sessions)",
	Long: `View the output history of a session without attaching.

Reads from the persistent session log, so it works for both active
and ended sessions. Useful for checking if a task completed.

Examples:
  hqssh logs abc12345                  # View full log
  hqssh logs abc12345 -H server        # Specify host`,
	Args: cobra.ExactArgs(1),
	RunE: runLogs,
}

func init() {
	rootCmd.AddCommand(logsCmd)
}

func runLogs(cmd *cobra.Command, args []string) error {
	sessionID := args[0]
	cfg := resolveConfig()

	if cfg.Host == "" {
		return fmt.Errorf("no host specified\n\nProvide a host using one of:\n  --host/-H flag:    hqssh logs %s -H server.example.com\n  Environment var:   export HQSSH_HOST=server.example.com\n  Config file:       ~/.hqssh/config.yaml with default_host set", sessionID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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

	// Resolve session ID (include ended sessions for logs)
	fullSessionID, err := resolveSessionIncludeEnded(ctx, c, sessionID)
	if err != nil {
		return fmt.Errorf("find session: %w", err)
	}

	// Stream session log (works for both active and ended sessions)
	stream, err := c.SessionService.GetSessionLog(ctx, &pb.GetSessionLogRequest{
		SessionId: fullSessionID,
	})
	if err != nil {
		return fmt.Errorf("get session log: %w", err)
	}

	hasOutput := false
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read session log: %w", err)
		}
		if len(chunk.Data) > 0 {
			hasOutput = true
			os.Stdout.Write(chunk.Data)
		}
	}

	if !hasOutput {
		fmt.Fprintf(os.Stderr, "No output captured yet.\n")
		return nil
	}

	return nil
}
