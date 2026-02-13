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

	"github.com/spf13/cobra"
)

var attachCmd = &cobra.Command{
	Use:   "attach <session-id>",
	Short: "Attach to a session for mobile-to-desktop handoff",
	Long: `Attach to an AI session running on the remote system.

This is the core handoff feature: start a session from your mobile device,
then continue it from your desktop.

The session will remain active after you detach (Ctrl-D or close terminal).
Other clients (including your mobile device) can remain connected.`,
	Args: cobra.ExactArgs(1),
	RunE: runAttach,
}

func init() {
	// Attach-specific flags could go here
}

func runAttach(cmd *cobra.Command, args []string) error {
	sessionID := args[0]

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

	// Find session (resolve short ID)
	fullSessionID, err := resolveSession(ctx, c, sessionID, hostLabel)
	if err != nil {
		return fmt.Errorf("find session: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Attaching to session %s...\n", shortID(fullSessionID))

	return attachToSession(ctx, cancel, c, fullSessionID, hostLabel)
}
