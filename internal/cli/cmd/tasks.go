package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/gelotto/hqsshd/internal/cli/client"
	pb "github.com/gelotto/hqsshd/proto"
	"github.com/spf13/cobra"
)

var (
	tasksProject string
	tasksJSON    bool
)

var tasksCmd = &cobra.Command{
	Use:   "tasks",
	Short: "List available tasks",
	Long: `List task definitions on the remote system.

Tasks are automated commands that can be run on-demand.
Use 'hqssh run <task-id>' to execute a task.

Examples:
  hqssh tasks                     # List all tasks
  hqssh tasks --project abc123    # Filter by project
  hqssh tasks --json              # Machine-readable output`,
	RunE: runTasks,
}

func init() {
	tasksCmd.Flags().StringVar(&tasksProject, "project", "", "Filter by project ID")
	tasksCmd.Flags().BoolVar(&tasksJSON, "json", false, "Output as JSON")
	rootCmd.AddCommand(tasksCmd)
}

func runTasks(cmd *cobra.Command, args []string) error {
	cfg := resolveConfig()

	if cfg.Host == "" {
		return fmt.Errorf("no host specified\n\nProvide a host using one of:\n  --host/-H flag:    hqssh tasks -H server.example.com\n  Environment var:   export HQSSH_HOST=server.example.com\n  Config file:       ~/.hqssh/config.yaml with default_host set")
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

	// List tasks
	resp, err := c.TaskService.List(ctx, &pb.ListTasksRequest{
		ProjectId: tasksProject,
	})
	if err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}

	// JSON output
	if tasksJSON {
		type jsonTask struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description,omitempty"`
			Tool        string `json:"tool"`
			Scope       string `json:"scope"`
			ProjectID   string `json:"project_id,omitempty"`
			Timeout     int32  `json:"timeout_seconds,omitempty"`
		}
		out := make([]jsonTask, len(resp.Tasks))
		for i, t := range resp.Tasks {
			out[i] = jsonTask{
				ID:          t.Id,
				Name:        t.Name,
				Description: t.Description,
				Tool:        t.Tool,
				Scope:       scopeString(t.Scope),
				ProjectID:   t.ProjectId,
				Timeout:     t.TimeoutSeconds,
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(resp.Tasks) == 0 {
		fmt.Println("No tasks defined.")
		fmt.Println("\nCreate tasks from the HQSSH mobile app to automate commands.")
		return nil
	}

	// Print tasks table
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tTOOL\tSCOPE\tDESCRIPTION")
	fmt.Fprintln(w, "--\t----\t----\t-----\t-----------")

	for _, t := range resp.Tasks {
		desc := t.Description
		if len(desc) > 40 {
			desc = desc[:37] + "..."
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			shortID(t.Id),
			truncate(t.Name, 20),
			t.Tool,
			scopeString(t.Scope),
			desc,
		)
	}
	w.Flush()

	fmt.Println("\nTo run a task:")
	fmt.Printf("  hqssh run <ID> -H %s\n", cfg.Host)

	return nil
}

func scopeString(s pb.TaskScope) string {
	switch s {
	case pb.TaskScope_TASK_SCOPE_GLOBAL:
		return "GLOBAL"
	case pb.TaskScope_TASK_SCOPE_SYSTEM:
		return "SYSTEM"
	case pb.TaskScope_TASK_SCOPE_PROJECT:
		return "PROJECT"
	default:
		return "UNKNOWN"
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
