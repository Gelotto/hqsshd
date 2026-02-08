package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/gelotto/hqsshd/internal/cli/client"
	pb "github.com/gelotto/hqsshd/proto"
	"github.com/spf13/cobra"
)

var cancelCmd = &cobra.Command{
	Use:   "cancel <run-id>",
	Short: "Cancel a running task",
	Long: `Cancel a currently running task by its run ID.

Examples:
  hqssh cancel abc12345
  hqssh runs                    # Find the run ID first`,
	Args: cobra.ExactArgs(1),
	RunE: runCancel,
}

func init() {
	rootCmd.AddCommand(cancelCmd)
}

func runCancel(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, hostLabel, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	runID, err := resolveRunID(ctx, c, args[0], hostLabel)
	if err != nil {
		return err
	}

	_, err = c.TaskService.CancelRun(ctx, &pb.CancelRunRequest{
		RunId: runID,
	})
	if err != nil {
		return fmt.Errorf("cancel run: %w", err)
	}

	fmt.Printf("Run cancelled: %s\n", shortID(runID))
	return nil
}

// resolveRunID finds a run by ID prefix.
func resolveRunID(ctx context.Context, c *client.Client, idPrefix, hostLabel string) (string, error) {
	resp, err := c.TaskService.ListRuns(ctx, &pb.ListRunsRequest{
		Limit: 100,
	})
	if err != nil {
		return "", err
	}

	var matches []*pb.TaskRun
	for _, r := range resp.Runs {
		if r.Id == idPrefix || (len(r.Id) >= len(idPrefix) && r.Id[:len(idPrefix)] == idPrefix) {
			matches = append(matches, r)
		}
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("run not found: %s\n\nRun 'hqssh runs%s' to list recent runs", idPrefix, hostFlag(hostLabel))
	}
	if len(matches) > 1 {
		msg := fmt.Sprintf("ambiguous run ID '%s' matches %d runs:\n", idPrefix, len(matches))
		for _, r := range matches {
			msg += fmt.Sprintf("  %s  task:%s  %s\n", shortID(r.Id), shortID(r.TaskId), runStatusString(r.Status))
		}
		msg += "\nUse a longer ID prefix to be specific"
		return "", fmt.Errorf("%s", msg)
	}

	return matches[0].Id, nil
}
