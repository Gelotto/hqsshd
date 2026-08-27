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
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/gelotto/hqsshd/internal/doctor"
	"github.com/spf13/cobra"
)

var (
	doctorJSON  bool
	doctorQuiet bool
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose the local hqsshd installation",
	Long: `Check the local hqsshd installation and explain what to fix.

Runs without a daemon. Checks the binaries, the background service (launchd
on macOS, systemd on Linux), the Unix socket and the TCP port the mobile app
uses, configuration, logs and the data directory. Every failed check comes
with the command that fixes it.

Exit status is 1 when any check fails, so scripts can use it as a health
check:

  hqssh doctor --quiet && echo healthy

The doctor is local only. For a remote host run it over SSH:

  ssh user@host hqssh doctor`,
	Args: cobra.NoArgs,
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false, "Output as JSON")
	doctorCmd.Flags().BoolVarP(&doctorQuiet, "quiet", "q", false, "No output; exit status only")
	rootCmd.AddCommand(doctorCmd)
}

func runDoctor(cmd *cobra.Command, args []string) error {
	if host != "" {
		return errors.New("doctor runs locally; for a remote host use: ssh <host> hqssh doctor")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	report := doctor.Run(ctx, doctor.Options{SocketPath: socket})

	switch {
	case doctorQuiet:
	case doctorJSON:
		if err := doctor.RenderJSON(os.Stdout, report); err != nil {
			return err
		}
	default:
		doctor.Render(os.Stdout, report)
	}

	if report.Failed() {
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		if !doctorQuiet && !doctorJSON {
			fmt.Fprintln(os.Stderr)
		}
		os.Exit(1)
	}
	return nil
}
