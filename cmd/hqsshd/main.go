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
		logging.Info("hqsshd stopped")
		os.Exit(0)
	}()

	// Start server
	logging.Info("hqsshd ready", "socket", cfg.Socket, "tcp_port", cfg.TCPPort)
	if err := srv.Start(); err != nil {
		logging.Error("server error", "error", err)
		os.Exit(1)
	}
}
