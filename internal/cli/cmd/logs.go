package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, hostLabel, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	// Resolve session ID (include ended sessions for logs)
	fullSessionID, err := resolveSessionIncludeEnded(ctx, c, sessionID, hostLabel)
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
