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
	"os/signal"
	"syscall"

	pb "github.com/gelotto/hqsshd/proto"
	"github.com/spf13/cobra"
)

var (
	newTool     string
	newProject  string
	newNoAttach bool
	newName     string
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
	newCmd.Flags().StringVarP(&newName, "name", "n", "", "Explicit session name (auto-generated if omitted)")
	rootCmd.AddCommand(newCmd)
}

func runNew(cmd *cobra.Command, args []string) error {
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

	c, hostLabel, err := connectDaemon(ctx)
	if err != nil {
		return err
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
		Name:             newName,
	})
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Session created: %s\n", shortID(session.Id))

	if newNoAttach {
		fmt.Fprintf(os.Stderr, "\nTo attach later:\n")
		fmt.Fprintf(os.Stderr, "  hqssh attach %s%s\n", shortID(session.Id), hostFlag(hostLabel))
		return nil
	}

	// Attach to the session (reuse shared attach logic)
	fmt.Fprintf(os.Stderr, "Attaching to session %s...\n", shortID(session.Id))

	return attachToSession(ctx, cancel, c, session.Id, hostLabel)
}
