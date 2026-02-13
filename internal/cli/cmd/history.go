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
	historyProject string
	historyLimit   int
	historyJSON    bool
)

var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "List ended sessions",
	Long: `Show historical sessions that have ended.

Displays ended sessions with their tool, duration, and log info.

Examples:
  hqssh history                        # Recent ended sessions
  hqssh history --project abc123       # Filter by project
  hqssh history --limit 100            # Show more entries
  hqssh history --json                 # Machine-readable output`,
	RunE: runHistory,
}

func init() {
	historyCmd.Flags().StringVar(&historyProject, "project", "", "Filter by project ID")
	historyCmd.Flags().IntVar(&historyLimit, "limit", 50, "Maximum entries to show")
	historyCmd.Flags().BoolVar(&historyJSON, "json", false, "Output as JSON")
	rootCmd.AddCommand(historyCmd)
}

func runHistory(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, _, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := c.SessionService.ListHistoricalSessions(ctx, &pb.ListHistoricalSessionsRequest{
		Limit:     int32(historyLimit),
		ProjectId: historyProject,
	})
	if err != nil {
		return fmt.Errorf("list history: %w", err)
	}

	if historyJSON {
		type jsonHistSession struct {
			ID           string `json:"id"`
			Name         string `json:"name,omitempty"`
			Tool         string `json:"tool"`
			WorkingDir   string `json:"working_directory"`
			ProjectID    string `json:"project_id,omitempty"`
			CreatedAt    int64  `json:"created_at"`
			EndedAt      int64  `json:"ended_at"`
			Duration     string `json:"duration"`
			LogSizeBytes int64  `json:"log_size_bytes"`
		}
		out := make([]jsonHistSession, len(resp.Sessions))
		for i, s := range resp.Sessions {
			out[i] = jsonHistSession{
				ID:           s.Id,
				Name:         s.Name,
				Tool:         s.Tool,
				WorkingDir:   s.WorkingDirectory,
				ProjectID:    s.ProjectId,
				CreatedAt:    s.CreatedAt,
				EndedAt:      s.EndedAt,
				Duration:     formatDuration(s.CreatedAt, s.EndedAt),
				LogSizeBytes: s.LogSizeBytes,
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(resp.Sessions) == 0 {
		fmt.Println("No session history found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tDURATION\tLOG SIZE\tENDED")
	fmt.Fprintln(w, "--\t----\t--------\t--------\t-----")

	for _, s := range resp.Sessions {
		name := s.Name
		if name == "" {
			name = s.Tool
		}
		if len(name) > 30 {
			name = name[:27] + "..."
		}

		logSize := formatBytes(s.LogSizeBytes)
		duration := formatDuration(s.CreatedAt, s.EndedAt)
		ended := formatTime(s.EndedAt)

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			shortID(s.Id),
			name,
			duration,
			logSize,
			ended,
		)
	}
	w.Flush()

	return nil
}

func formatBytes(b int64) string {
	if b == 0 {
		return "-"
	}
	if b < 1024 {
		return fmt.Sprintf("%dB", b)
	}
	if b < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(b)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(b)/(1024*1024))
}
