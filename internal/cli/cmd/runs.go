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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	pb "github.com/gelotto/hqsshd/proto"
	"github.com/spf13/cobra"
)

var (
	runsTask  string
	runsLimit int
	runsJSON  bool
)

var runsCmd = &cobra.Command{
	Use:   "runs",
	Short: "List task run history",
	Long: `Show the execution history of tasks.

Lists recent task runs with their status, duration, and timestamps.
Use --task to filter runs for a specific task.

Examples:
  hqssh runs                      # Recent runs across all tasks
  hqssh runs --task abc12345      # Runs for specific task
  hqssh runs --limit 100          # Show more runs
  hqssh runs --json               # Machine-readable output`,
	RunE: runRuns,
}

func init() {
	runsCmd.Flags().StringVar(&runsTask, "task", "", "Filter by task ID")
	runsCmd.Flags().IntVar(&runsLimit, "limit", 50, "Maximum runs to show")
	runsCmd.Flags().BoolVar(&runsJSON, "json", false, "Output as JSON")
	rootCmd.AddCommand(runsCmd)
}

func runRuns(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, hostLabel, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	// Resolve task ID if specified
	taskFilter := runsTask
	if taskFilter != "" {
		fullTaskID, err := resolveTaskID(ctx, c, taskFilter, hostLabel)
		if err != nil {
			return fmt.Errorf("find task: %w", err)
		}
		taskFilter = fullTaskID
	}

	// List runs
	resp, err := c.TaskService.ListRuns(ctx, &pb.ListRunsRequest{
		TaskId: taskFilter,
		Limit:  int32(runsLimit),
	})
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}

	// JSON output
	if runsJSON {
		type jsonRun struct {
			ID          string `json:"id"`
			TaskID      string `json:"task_id"`
			Status      string `json:"status"`
			StartedAt   int64  `json:"started_at"`
			CompletedAt int64  `json:"completed_at,omitempty"`
			Duration    string `json:"duration"`
		}
		out := make([]jsonRun, len(resp.Runs))
		for i, r := range resp.Runs {
			out[i] = jsonRun{
				ID:          r.Id,
				TaskID:      r.TaskId,
				Status:      runStatusString(r.Status),
				StartedAt:   r.StartedAt,
				CompletedAt: r.CompletedAt,
				Duration:    formatDuration(r.StartedAt, r.CompletedAt),
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(resp.Runs) == 0 {
		fmt.Println("No task runs found.")
		fmt.Println("\nRun a task with:")
		fmt.Printf("  hqssh run <task-id>%s\n", hostFlag(hostLabel))
		return nil
	}

	// Print runs table
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RUN ID\tTASK\tSTATUS\tDURATION\tSTARTED")
	fmt.Fprintln(w, "------\t----\t------\t--------\t-------")

	for _, r := range resp.Runs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			shortID(r.Id),
			shortID(r.TaskId),
			runStatusString(r.Status),
			formatDuration(r.StartedAt, r.CompletedAt),
			formatTime(r.StartedAt),
		)
	}
	w.Flush()

	return nil
}
