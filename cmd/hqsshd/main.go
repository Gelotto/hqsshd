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

// Command hqsshd is the HQSSH daemon.
//
// Exit codes (service managers restart any non-zero exit, so each one is
// also logged with the fix):
//
//	0  clean shutdown (SIGTERM/SIGINT)
//	1  runtime error while serving
//	2  configuration error (bad YAML, invalid values, bad flag)
//	3  already running (another hqsshd owns the socket or pidfile)
//	4  bind failure (socket path unusable, TCP port still in use)
//	5  log file cannot be created or opened
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gelotto/hqsshd/internal/config"
	"github.com/gelotto/hqsshd/internal/logging"
	"github.com/gelotto/hqsshd/internal/server"
	"github.com/gelotto/hqsshd/internal/shellutil"
)

const (
	exitOK             = 0
	exitRuntime        = 1
	exitConfig         = 2
	exitAlreadyRunning = 3
	exitBind           = 4
	exitLogging        = 5
)

func main() {
	// Parse flags
	configPath := flag.String("config", "", "Path to config file")
	socketPath := flag.String("socket", "", "Override socket path")
	logLevel := flag.String("log-level", "", "Override log level (debug, info, warn, error)")
	version := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	// Print version
	if *version {
		fmt.Printf("hqsshd version %s\n", config.DaemonVersion)
		os.Exit(exitOK)
	}

	// Load configuration
	var cfg *config.Config
	var err error
	if *configPath != "" {
		cfg, err = config.LoadFromPath(*configPath)
	} else {
		cfg, err = config.Load()
	}
	if err != nil {
		fatal(exitConfig, "failed to load config", err)
	}

	// Check config file permissions (may contain auth_token)
	if *configPath != "" {
		if err := config.CheckFilePermissions(*configPath); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
		}
	} else if defaultPath, err := config.DefaultConfigPath(); err == nil {
		if err := config.CheckFilePermissions(defaultPath); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
		}
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		fatal(exitConfig, "invalid config", err)
	}

	// Override socket path if provided
	if *socketPath != "" {
		cfg.Socket = *socketPath
	}

	// Override log level if provided
	if *logLevel != "" {
		cfg.Log.Level = *logLevel
	}

	// Initialize logging
	if err := logging.Init(&logging.Config{
		Level:  cfg.Log.Level,
		Format: cfg.Log.Format,
		File:   cfg.Log.File,
	}); err != nil {
		fatal(exitLogging, "failed to initialize logging", err)
	}

	logging.Info("hqsshd starting",
		"version", config.DaemonVersion,
		"commit", config.Commit,
		"pid", os.Getpid(),
		"socket", cfg.Socket,
		"tcp_port", cfg.TCPPort,
		"log_level", cfg.Log.Level,
	)

	// Service managers (launchd, systemd) start the daemon with a minimal
	// PATH that misses per-user tool installs (e.g. claude in ~/.local/bin).
	shellutil.EnsureUserPATH()

	// Create server (runs the single-instance check first)
	srv, err := server.NewServer(cfg)
	if err != nil {
		fatal(exitCodeFor(err), "startup failed", err)
	}

	// Detect AI tools before answering anyone: the app's first request is
	// GetInfo, and a cold probe (login shells) must not make it time out.
	srv.Detector().Warm()

	// Handle shutdown signals. Stop is safe before Listen, so a signal that
	// lands during startup still shuts down cleanly.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		logging.Info("received shutdown signal", "signal", sig.String())
		srv.Stop()
	}()

	// Bind first; only then claim to be ready.
	if err := srv.Listen(); err != nil {
		if errors.Is(err, server.ErrShuttingDown) {
			logging.Info("hqsshd stopped before startup completed")
			logging.Close()
			os.Exit(exitOK)
		}
		fatal(exitCodeFor(err), "startup failed", err)
	}
	sock, tcp := srv.Addrs()
	switch {
	case tcp != "":
	case srv.TCPPending() != "":
		tcp = "pending (" + srv.TCPPending() + " in use; retrying)"
	default:
		tcp = "disabled"
	}
	logging.Info("hqsshd ready", "socket", sock, "tcp", tcp, "pid", os.Getpid())

	// Serve blocks until Stop() is called or an error occurs
	if err := srv.Serve(); err != nil {
		fatal(exitRuntime, "server error", err)
	}

	// Serve returns as soon as the listeners drain, while the signal
	// goroutine's Stop() may still be releasing the pidfile and writing the
	// final log line. Stop is idempotent and blocks until the in-flight
	// shutdown completes, so this is a join, not a second shutdown.
	srv.Stop()

	logging.Close()
	os.Exit(exitOK)
}

// fatal reports a startup or runtime failure and exits with code. The
// message goes to the log when the logger is up and to stderr when the
// logger is not up or writes to a file, so it is visible both in
// ~/.hqssh/logs/hqsshd.log (launchd), journald (systemd) and a terminal.
func fatal(code int, msg string, err error) {
	if logging.Logger != nil {
		logging.Error(msg, "error", err, "exit_code", code)
	}
	if logging.Logger == nil || logging.ToFile() {
		fmt.Fprintf(os.Stderr, "hqsshd: %s: %v\n", msg, err)
	}
	logging.Close()
	os.Exit(code)
}

// exitCodeFor maps a Listen error to its exit code.
func exitCodeFor(err error) int {
	var already *server.AlreadyRunningError
	if errors.As(err, &already) {
		return exitAlreadyRunning
	}
	var bind *server.BindError
	if errors.As(err, &bind) {
		return exitBind
	}
	return exitRuntime
}
