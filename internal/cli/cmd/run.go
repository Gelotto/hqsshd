package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gelotto/hqsshd/internal/cli/client"
	pb "github.com/gelotto/hqsshd/proto"
	"github.com/spf13/cobra"
)

var (
	runAsync bool
	runJSON  bool
)

var runCmd = &cobra.Command{
	Use:   "run <task-id>",
	Short: "Execute a task",
	Long: `Run a task on the remote system.

By default, waits for the task to complete and shows the output.
Use --async to start the task and return immediately.

Examples:
  hqssh run abc12345                    # Run and wait for completion
  hqssh run abc12345 --async            # Start and return immediately
  hqssh run abc12345 --json             # Output result as JSON`,
	Args: cobra.ExactArgs(1),
	RunE: runRunTask,
}

func init() {
	runCmd.Flags().BoolVar(&runAsync, "async", false, "Start task and return immediately")
	runCmd.Flags().BoolVar(&runJSON, "json", false, "Output as JSON")
	rootCmd.AddCommand(runCmd)
}

func runRunTask(cmd *cobra.Command, args []string) error {
	taskID := args[0]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle Ctrl-C
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)
	go func() {
		<-sigChan
		fmt.Fprintf(os.Stderr, "\nCancelling...\n")
		cancel()
	}()

	c, hostLabel, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	// Resolve task ID (try to find by prefix)
	fullTaskID, err := resolveTaskID(ctx, c, taskID, hostLabel)
	if err != nil {
		return fmt.Errorf("find task: %w", err)
	}

	// Run the task
	fmt.Fprintf(os.Stderr, "Running task %s...\n", shortID(fullTaskID))
	run, err := c.TaskService.Run(ctx, &pb.RunTaskRequest{
		TaskId: fullTaskID,
	})
	if err != nil {
		return fmt.Errorf("run task: %w", err)
	}

	if runAsync {
		fmt.Printf("Task started. Run ID: %s\n", shortID(run.Id))
		fmt.Printf("\nCheck status with:\n")
		fmt.Printf("  hqssh runs --task %s%s\n", shortID(fullTaskID), hostFlag(hostLabel))
		return nil
	}

	// Poll for completion
	fmt.Fprintf(os.Stderr, "Waiting for completion...\n")
	for {
		select {
		case <-ctx.Done():
			// Try to cancel the run
			cancelCtx, cancelCancel := context.WithTimeout(context.Background(), 5*time.Second)
			c.TaskService.CancelRun(cancelCtx, &pb.CancelRunRequest{RunId: run.Id})
			cancelCancel()
			return fmt.Errorf("cancelled")
		case <-time.After(1 * time.Second):
		}

		run, err = c.TaskService.GetRun(ctx, &pb.GetRunRequest{RunId: run.Id})
		if err != nil {
			return fmt.Errorf("get run status: %w", err)
		}

		if run.Status == pb.TaskRunStatus_TASK_RUN_STATUS_COMPLETED ||
			run.Status == pb.TaskRunStatus_TASK_RUN_STATUS_FAILED ||
			run.Status == pb.TaskRunStatus_TASK_RUN_STATUS_CANCELLED {
			break
		}
	}

	// Output result
	if runJSON {
		type jsonRun struct {
			ID          string `json:"id"`
			TaskID      string `json:"task_id"`
			Status      string `json:"status"`
			StartedAt   int64  `json:"started_at"`
			CompletedAt int64  `json:"completed_at"`
			Duration    string `json:"duration"`
			Output      string `json:"output,omitempty"`
			Error       string `json:"error,omitempty"`
		}
		out := jsonRun{
			ID:          run.Id,
			TaskID:      run.TaskId,
			Status:      runStatusString(run.Status),
			StartedAt:   run.StartedAt,
			CompletedAt: run.CompletedAt,
			Duration:    formatDuration(run.StartedAt, run.CompletedAt),
			Output:      run.Output,
			Error:       run.Error,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	// Human-readable output
	fmt.Printf("\n--- Output ---\n")
	if run.Output != "" {
		fmt.Print(run.Output)
		if run.Output[len(run.Output)-1] != '\n' {
			fmt.Println()
		}
	}
	if run.Error != "" {
		fmt.Fprintf(os.Stderr, "\n--- Error ---\n%s\n", run.Error)
	}

	fmt.Printf("\n--- Result ---\n")
	fmt.Printf("Status:   %s\n", runStatusString(run.Status))
	fmt.Printf("Duration: %s\n", formatDuration(run.StartedAt, run.CompletedAt))

	if run.Status == pb.TaskRunStatus_TASK_RUN_STATUS_FAILED {
		return fmt.Errorf("task failed")
	}

	return nil
}

func resolveTaskID(ctx context.Context, c *client.Client, idPrefix, hostLabel string) (string, error) {
	resp, err := c.TaskService.List(ctx, &pb.ListTasksRequest{})
	if err != nil {
		return "", err
	}

	var matches []*pb.Task
	for _, t := range resp.Tasks {
		if t.Id == idPrefix || (len(t.Id) >= len(idPrefix) && t.Id[:len(idPrefix)] == idPrefix) {
			matches = append(matches, t)
		}
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("task not found: %s\n\nRun 'hqssh tasks%s' to list available tasks", idPrefix, hostFlag(hostLabel))
	}
	if len(matches) > 1 {
		var msg string
		msg = fmt.Sprintf("ambiguous task ID '%s' matches %d tasks:\n", idPrefix, len(matches))
		for _, t := range matches {
			msg += fmt.Sprintf("  %s  %s  %s\n", shortID(t.Id), t.Name, t.Tool)
		}
		msg += "\nUse a longer ID prefix to be specific"
		return "", fmt.Errorf("%s", msg)
	}

	return matches[0].Id, nil
}

func runStatusString(s pb.TaskRunStatus) string {
	switch s {
	case pb.TaskRunStatus_TASK_RUN_STATUS_PENDING:
		return "PENDING"
	case pb.TaskRunStatus_TASK_RUN_STATUS_RUNNING:
		return "RUNNING"
	case pb.TaskRunStatus_TASK_RUN_STATUS_COMPLETED:
		return "COMPLETED"
	case pb.TaskRunStatus_TASK_RUN_STATUS_FAILED:
		return "FAILED"
	case pb.TaskRunStatus_TASK_RUN_STATUS_CANCELLED:
		return "CANCELLED"
	default:
		return "UNKNOWN"
	}
}

func formatDuration(startedAt, completedAt int64) string {
	if startedAt == 0 {
		return "-"
	}
	end := completedAt
	if end == 0 {
		end = time.Now().Unix()
	}
	d := time.Duration(end-startedAt) * time.Second
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}
