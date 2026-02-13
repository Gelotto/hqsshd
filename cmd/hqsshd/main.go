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

package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gelotto/hqsshd/internal/config"
	"github.com/gelotto/hqsshd/internal/logging"
	"github.com/gelotto/hqsshd/internal/server"
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
		os.Exit(0)
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
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Check config file permissions (may contain auth_token)
	if *configPath != "" {
		if err := config.CheckFilePermissions(*configPath); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
		}
	} else {
		homeDir, _ := os.UserHomeDir()
		if homeDir != "" {
			defaultPath := homeDir + "/" + config.DefaultConfigDir + "/" + config.DefaultConfigFile
			if err := config.CheckFilePermissions(defaultPath); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
			}
		}
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid config: %v\n", err)
		os.Exit(1)
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
		fmt.Fprintf(os.Stderr, "Failed to initialize logging: %v\n", err)
		os.Exit(1)
	}
	defer logging.Close()

	// Log startup
	logging.Info("hqsshd starting",
		"version", config.DaemonVersion,
		"socket", cfg.Socket,
		"tcp_port", cfg.TCPPort,
		"log_level", cfg.Log.Level,
		"pid", os.Getpid(),
	)

	// Create server
	srv, err := server.NewServer(cfg)
	if err != nil {
		logging.Error("failed to create server", "error", err)
		os.Exit(1)
	}

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		logging.Info("received shutdown signal", "signal", sig.String())
		srv.Stop()
	}()

	// Start server (blocks until Stop() is called or an error occurs)
	logging.Info("hqsshd ready", "socket", cfg.Socket, "tcp_port", cfg.TCPPort)
	if err := srv.Start(); err != nil {
		logging.Error("server error", "error", err)
		os.Exit(1)
	}

	logging.Info("hqsshd stopped")
}
