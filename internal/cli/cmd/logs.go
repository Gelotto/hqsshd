package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/gelotto/hqsshd/internal/cli/client"
	pb "github.com/gelotto/hqsshd/proto"
	"github.com/spf13/cobra"
)

var (
	logsLines int
)

var logsCmd = &cobra.Command{
	Use:   "logs <session-id>",
	Short: "View session scrollback buffer",
	Long: `View the output history of a session without attaching.

Shows the terminal output that has been captured in the session's
scrollback buffer. Useful for checking if a task completed.

Examples:
  hqssh logs abc12345                  # View full scrollback
  hqssh logs abc12345 -n 100           # Last 100 lines
  hqssh logs abc12345 -H server        # Specify host`,
	Args: cobra.ExactArgs(1),
	RunE: runLogs,
}

func init() {
	logsCmd.Flags().IntVarP(&logsLines, "lines", "n", 0, "Number of lines (0 = all)")
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

	// Resolve session ID
	fullSessionID, err := resolveSessionIDForLogs(ctx, c, sessionID)
	if err != nil {
		return fmt.Errorf("find session: %w", err)
	}

	// Get scrollback
	resp, err := c.SessionService.GetScrollback(ctx, &pb.GetScrollbackRequest{
		SessionId: fullSessionID,
		Lines:     int32(logsLines),
	})
	if err != nil {
		return fmt.Errorf("get scrollback: %w", err)
	}

	// Output the scrollback data
	if len(resp.Data) == 0 {
		fmt.Fprintf(os.Stderr, "No output captured yet.\n")
		return nil
	}

	os.Stdout.Write(resp.Data)

	// Ensure newline at end
	if len(resp.Data) > 0 && resp.Data[len(resp.Data)-1] != '\n' {
		fmt.Println()
	}

	return nil
}

// resolveSessionIDForLogs is like resolveSessionID but includes ended sessions
func resolveSessionIDForLogs(ctx context.Context, c *client.Client, idPrefix string) (string, error) {
	resp, err := c.SessionService.List(ctx, &pb.ListSessionsRequest{IncludeEnded: true})
	if err != nil {
		return "", err
	}

	var matches []*pb.Session
	for _, s := range resp.Sessions {
		if s.Id == idPrefix || (len(s.Id) >= len(idPrefix) && s.Id[:len(idPrefix)] == idPrefix) {
			matches = append(matches, s)
		}
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("session not found: %s\n\nRun 'hqssh sessions --all' to list all sessions including ended ones", idPrefix)
	}
	if len(matches) > 1 {
		var msg string
		msg = fmt.Sprintf("ambiguous session ID '%s' matches %d sessions:\n", idPrefix, len(matches))
		for _, m := range matches {
			project := m.ProjectId
			if project == "" {
				project = "(shell)"
			}
			msg += fmt.Sprintf("  %s  %s  %s\n", shortID(m.Id), m.Tool, project)
		}
		msg += "\nUse a longer ID prefix to be specific"
		return "", fmt.Errorf("%s", msg)
	}

	return matches[0].Id, nil
}
