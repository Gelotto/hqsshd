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
	"fmt"
	"os"
	"time"

	pb "github.com/gelotto/hqsshd/proto"
	"github.com/spf13/cobra"
)

var (
	taskCreateName        string
	taskCreateDescription string
	taskCreateTool        string
	taskCreateScope       string
	taskCreateProject     string
	taskCreatePrompt      string
	taskCreateTimeout     int

	taskUpdateName        string
	taskUpdateDescription string
	taskUpdateTool        string
	taskUpdateScope       string
	taskUpdateProject     string
	taskUpdatePrompt      string
	taskUpdateTimeout     int

	taskDeleteForce bool
)

var taskCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new task definition",
	Long: `Create a new task definition on the remote system.

Examples:
  hqssh task create --name "Deploy" --tool shell --scope system --prompt "make deploy"
  hqssh task create --name "Review" --tool claude --scope project --project abc123 --prompt "Review this code"`,
	RunE: runTaskCreate,
}

var taskUpdateCmd = &cobra.Command{
	Use:   "update <id>",
	Short: "Update a task definition",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskUpdate,
}

var taskDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete a task definition",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskDelete,
}

// taskCmd is a parent for create/update/delete subcommands
var taskCmd = &cobra.Command{
	Use:   "task",
	Short: "Task management (create, update, delete)",
	Long: `Manage task definitions on the remote system.

Use 'hqssh tasks' to list tasks and 'hqssh run' to execute them.

Examples:
  hqssh task create --name "Deploy" --tool shell --scope system --prompt "make deploy"
  hqssh task update abc123 --name "New Name"
  hqssh task delete abc123`,
}

func init() {
	// Create flags
	taskCreateCmd.Flags().StringVar(&taskCreateName, "name", "", "Task name (required)")
	taskCreateCmd.Flags().StringVar(&taskCreateDescription, "description", "", "Task description")
	taskCreateCmd.Flags().StringVar(&taskCreateTool, "tool", "shell", "Tool: claude, codex, aider, shell")
	taskCreateCmd.Flags().StringVar(&taskCreateScope, "scope", "system", "Scope: system, project")
	taskCreateCmd.Flags().StringVar(&taskCreateProject, "project", "", "Project ID (required for project scope)")
	taskCreateCmd.Flags().StringVar(&taskCreatePrompt, "prompt", "", "Command or prompt to execute")
	taskCreateCmd.Flags().IntVar(&taskCreateTimeout, "timeout", 0, "Timeout in seconds (0 = no timeout)")
	taskCreateCmd.MarkFlagRequired("name")

	// Update flags
	taskUpdateCmd.Flags().StringVar(&taskUpdateName, "name", "", "New task name")
	taskUpdateCmd.Flags().StringVar(&taskUpdateDescription, "description", "", "New description")
	taskUpdateCmd.Flags().StringVar(&taskUpdateTool, "tool", "", "New tool")
	taskUpdateCmd.Flags().StringVar(&taskUpdateScope, "scope", "", "New scope: system, project")
	taskUpdateCmd.Flags().StringVar(&taskUpdateProject, "project", "", "New project ID")
	taskUpdateCmd.Flags().StringVar(&taskUpdatePrompt, "prompt", "", "New prompt")
	taskUpdateCmd.Flags().IntVar(&taskUpdateTimeout, "timeout", 0, "New timeout in seconds")

	// Delete flags
	taskDeleteCmd.Flags().BoolVarP(&taskDeleteForce, "force", "f", false, "Skip confirmation")

	taskCmd.AddCommand(taskCreateCmd)
	taskCmd.AddCommand(taskUpdateCmd)
	taskCmd.AddCommand(taskDeleteCmd)
	rootCmd.AddCommand(taskCmd)
}

func runTaskCreate(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, _, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	scope := parseScope(taskCreateScope)
	if scope == pb.TaskScope_TASK_SCOPE_UNSPECIFIED {
		return fmt.Errorf("invalid scope: %s (use 'system' or 'project')", taskCreateScope)
	}

	if scope == pb.TaskScope_TASK_SCOPE_PROJECT && taskCreateProject == "" {
		return fmt.Errorf("--project is required when scope is 'project'")
	}

	task, err := c.TaskService.Create(ctx, &pb.CreateTaskRequest{
		Name:           taskCreateName,
		Description:    taskCreateDescription,
		Tool:           taskCreateTool,
		Scope:          scope,
		ProjectId:      taskCreateProject,
		Prompt:         taskCreatePrompt,
		TimeoutSeconds: int32(taskCreateTimeout),
	})
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}

	fmt.Printf("Task created: %s (%s)\n", task.Name, shortID(task.Id))
	fmt.Printf("  Tool:  %s\n", task.Tool)
	fmt.Printf("  Scope: %s\n", scopeString(task.Scope))
	if task.Prompt != "" {
		prompt := task.Prompt
		if len(prompt) > 60 {
			prompt = prompt[:57] + "..."
		}
		fmt.Printf("  Prompt: %s\n", prompt)
	}

	return nil
}

func runTaskUpdate(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, hostLabel, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	taskID, err := resolveTaskID(ctx, c, args[0], hostLabel)
	if err != nil {
		return err
	}

	scope := pb.TaskScope_TASK_SCOPE_UNSPECIFIED
	if taskUpdateScope != "" {
		scope = parseScope(taskUpdateScope)
		if scope == pb.TaskScope_TASK_SCOPE_UNSPECIFIED {
			return fmt.Errorf("invalid scope: %s (use 'system' or 'project')", taskUpdateScope)
		}
	}

	task, err := c.TaskService.Update(ctx, &pb.UpdateTaskRequest{
		Id:             taskID,
		Name:           taskUpdateName,
		Description:    taskUpdateDescription,
		Tool:           taskUpdateTool,
		Scope:          scope,
		ProjectId:      taskUpdateProject,
		Prompt:         taskUpdatePrompt,
		TimeoutSeconds: int32(taskUpdateTimeout),
	})
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}

	fmt.Printf("Task updated: %s (%s)\n", task.Name, shortID(task.Id))
	return nil
}

func runTaskDelete(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, hostLabel, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	taskID, err := resolveTaskID(ctx, c, args[0], hostLabel)
	if err != nil {
		return err
	}

	if !taskDeleteForce {
		fmt.Fprintf(os.Stderr, "Delete task %s? This also removes all run history. [y/N] ", shortID(taskID))
		var confirm string
		fmt.Scanln(&confirm)
		if confirm != "y" && confirm != "Y" {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	_, err = c.TaskService.Delete(ctx, &pb.DeleteTaskRequest{
		TaskId: taskID,
	})
	if err != nil {
		return fmt.Errorf("delete task: %w", err)
	}

	fmt.Printf("Task deleted: %s\n", shortID(taskID))
	return nil
}

func parseScope(s string) pb.TaskScope {
	switch s {
	case "system":
		return pb.TaskScope_TASK_SCOPE_SYSTEM
	case "project":
		return pb.TaskScope_TASK_SCOPE_PROJECT
	case "global":
		return pb.TaskScope_TASK_SCOPE_GLOBAL
	default:
		return pb.TaskScope_TASK_SCOPE_UNSPECIFIED
	}
}
