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
	"path/filepath"
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

By default, launches the first AI tool the daemon detects and attaches to it
immediately. Use --tool to choose one; "shell" needs enable_shell_tool: true
in the daemon's daemon.yaml.

Examples:
  hqssh new                              # First detected AI tool
  hqssh new --tool claude                # New Claude session
  hqssh new --tool claude                # In the current directory (local daemon)
  hqssh new --tool claude -d ~/src/app   # In a specific directory
  hqssh new --tool aider --no-attach     # Create but don't attach`,
	RunE: runNew,
}

func init() {
	newCmd.Flags().StringVarP(&newTool, "tool", "t", "", "Tool to launch: claude, codex, aider, shell (default: first detected AI tool)")
	newCmd.Flags().StringVarP(&newProject, "project", "d", "", "Working directory (default: current directory locally, home directory on a remote host)")
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

	workingDir, err := sessionWorkingDir(newProject, hostLabel == "")
	if err != nil {
		return err
	}

	tool, err := resolveTool(ctx, c, newTool)
	if err != nil {
		return err
	}

	// Create the session
	fmt.Fprintf(os.Stderr, "Creating %s session...\n", tool)
	session, err := c.SessionService.Create(ctx, &pb.CreateSessionRequest{
		Tool:             tool,
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

// sessionWorkingDir picks the directory to send with Create. A local daemon
// shares this filesystem, so an empty or relative dir means the caller's
// current directory, like any other command. For a remote host the value is
// sent as-is: the local cwd means nothing there, and the daemon resolves
// empty and relative paths against the remote user's home.
func sessionWorkingDir(dir string, local bool) (string, error) {
	if !local {
		return dir, nil
	}
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve working directory %q: %w", dir, err)
	}
	return abs, nil
}
