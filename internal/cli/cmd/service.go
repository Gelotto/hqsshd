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
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/gelotto/hqsshd/internal/servicemgr"
	"github.com/spf13/cobra"
)

var (
	serviceLogsFollow bool
	serviceLogsLines  int
)

var serviceCmd = &cobra.Command{
	Use:   "service",
	Short: "Manage the local hqsshd background service",
	Long: `Start, stop, restart and inspect the hqsshd background service without
remembering launchctl (macOS) or systemctl (Linux) incantations.

The service is installed by the installer:
  ` + servicemgr.InstallCommand + `

Commands:
  hqssh service status      # service manager state + daemon health
  hqssh service start
  hqssh service stop        # stays stopped until started again
  hqssh service restart     # new process; sessions are ended
  hqssh service logs -f     # daemon log (tail on macOS, journalctl on Linux)

Raw equivalents are shown by 'hqssh service status'.`,
}

var serviceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show service and daemon state",
	Args:  cobra.NoArgs,
	RunE:  runServiceStatus,
}

var serviceStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the service",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, args []string) error { return runServiceOp(cmd, "start") },
}

var serviceStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the service",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, args []string) error { return runServiceOp(cmd, "stop") },
}

var serviceRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the service",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, args []string) error { return runServiceOp(cmd, "restart") },
}

var serviceLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show the daemon log",
	Args:  cobra.NoArgs,
	RunE:  runServiceLogs,
}

func init() {
	serviceLogsCmd.Flags().BoolVarP(&serviceLogsFollow, "follow", "f", false, "Follow the log")
	serviceLogsCmd.Flags().IntVarP(&serviceLogsLines, "lines", "n", 50, "Number of lines to show")
	serviceCmd.AddCommand(serviceStatusCmd, serviceStartCmd, serviceStopCmd, serviceRestartCmd, serviceLogsCmd)
	rootCmd.AddCommand(serviceCmd)
}

// localService resolves the installed service or explains how to get one.
func localService() (servicemgr.Service, error) {
	if host != "" {
		return servicemgr.Service{}, errors.New("service commands run locally; for a remote host use: ssh <host> hqssh service ...")
	}
	svc, ok := servicemgr.Detect()
	if !ok {
		return servicemgr.Service{}, fmt.Errorf("no hqsshd service is installed on this machine\n\n"+
			"  Install:   %s\n"+
			"  Or run it in the foreground:   hqsshd", servicemgr.InstallCommand)
	}
	return svc, nil
}

func runServiceStatus(cmd *cobra.Command, args []string) error {
	svc, err := localService()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	info, err := svc.Status(ctx, servicemgr.ExecRunner{})
	if err != nil {
		return err
	}

	fmt.Printf("Service:  %s (%s)\n", svc, svc.UnitPath)
	fmt.Printf("State:    %s\n", info.Summary)
	for _, p := range info.Problems {
		fmt.Printf("          ! %s\n", p)
	}

	socketPath := localSocketPath()
	daemonOK := false
	if di, st, err := queryLocalDaemon(ctx, socketPath); err == nil {
		fmt.Printf("Daemon:   hqsshd %s %s, up %s, %d session(s)   (socket %s)\n",
			di.GetDaemonVersion(), pidLabel(st.GetPid()), formatUptime(st.GetUptimeSeconds()),
			st.GetActiveSessions(), socketPath)
		daemonOK = true
	} else {
		fmt.Printf("Daemon:   not answering on %s\n", socketPath)
	}

	tcpAddr := localTCPAddr()
	switch {
	case tcpAddr == "":
		fmt.Println("TCP:      disabled (tcp_port: 0 — the mobile app cannot connect)")
	default:
		if conn, err := net.DialTimeout("tcp", tcpAddr, 2*time.Second); err == nil {
			conn.Close()
			fmt.Printf("TCP:      %s ok\n", tcpAddr)
		} else {
			fmt.Printf("TCP:      %s not listening (the mobile app needs this)\n", tcpAddr)
		}
	}

	fmt.Printf("Logs:     %s\n", svc.LogsCommand())
	fmt.Printf("Manual:   start: %s\n          stop: %s\n          restart: %s\n",
		svc.StartCommand(), svc.StopCommand(), svc.RestartCommand())

	if !info.Running || !daemonOK {
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		os.Exit(1)
	}
	return nil
}

func runServiceOp(cmd *cobra.Command, op string) error {
	svc, err := localService()
	if err != nil {
		return err
	}
	cmd.SilenceUsage = true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r := servicemgr.ExecRunner{}

	if svc.NeedsSudo() {
		fmt.Fprintf(os.Stderr, "(%s lives in the system domain; launchctl runs under sudo)\n", svc)
	}

	switch op {
	case "start":
		fmt.Fprintf(os.Stderr, "Starting %s...\n", svc)
		if err := svc.Start(ctx, r); err != nil {
			return err
		}
	case "stop":
		fmt.Fprintf(os.Stderr, "Stopping %s...\n", svc)
		if err := svc.Stop(ctx, r); err != nil {
			return err
		}
		fmt.Println("hqsshd stopped (it stays stopped until `hqssh service start`)")
		return nil
	case "restart":
		fmt.Fprintf(os.Stderr, "Restarting %s...\n", svc)
		if err := svc.Restart(ctx, r); err != nil {
			return err
		}
	}

	// The service manager has a process; make sure it answers.
	socketPath := localSocketPath()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, st, err := queryLocalDaemon(ctx, socketPath); err == nil {
			tcp := localTCPAddr()
			switch {
			case tcp == "":
				tcp = "disabled"
			default:
				if conn, err := net.DialTimeout("tcp", tcp, time.Second); err == nil {
					conn.Close()
				} else {
					tcp += " (not listening — port in use? see: hqssh service logs -n 20)"
				}
			}
			fmt.Printf("hqsshd started (%s): socket %s, tcp %s\n", pidLabel(st.GetPid()), socketPath, tcp)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service started but the daemon is not answering on %s after 10s; check: hqssh service logs -n 50", socketPath)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func runServiceLogs(cmd *cobra.Command, args []string) error {
	svc, err := localService()
	if err != nil {
		return err
	}
	cmd.SilenceUsage = true
	name, cmdArgs := svc.LogsCommandArgs(serviceLogsFollow, serviceLogsLines)
	if svc.Kind == servicemgr.KindLaunchd {
		if _, err := os.Stat(cmdArgs[len(cmdArgs)-1]); err != nil {
			return fmt.Errorf("no daemon log at %s yet (the daemon has not started under launchd)", cmdArgs[len(cmdArgs)-1])
		}
	}
	c := exec.Command(name, cmdArgs...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}
