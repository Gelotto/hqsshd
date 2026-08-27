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

// Package logging provides structured logging for the hqsshd daemon.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

var (
	// Logger is the global structured logger.
	Logger *slog.Logger

	// logFile is the open log file (if any) for cleanup.
	logFile *os.File
)

// Config holds logging configuration.
type Config struct {
	Level  string // debug, info, warn, error
	Format string // json, text
	File   string // Log file path (empty = stdout)
}

// Init initializes the global logger with the given configuration.
// If config is nil, defaults to info level with text format to stdout.
func Init(cfg *Config) error {
	if cfg == nil {
		cfg = &Config{
			Level:  "info",
			Format: "text",
			File:   "",
		}
	}

	var output io.Writer = os.Stdout

	// If file is specified, open it for logging. The parent directory is
	// created on demand: a log.file pointing at a not-yet-existing directory
	// used to make the daemon exit before writing a single line, which under
	// launchd/systemd KeepAlive became a silent restart loop.
	if cfg.File != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.File), 0700); err != nil {
			return fmt.Errorf("create log directory for %s: %w", cfg.File, err)
		}
		f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("open log file %s: %w", cfg.File, err)
		}
		logFile = f
		output = f
	}

	level := parseLevel(cfg.Level)

	var handler slog.Handler
	opts := &slog.HandlerOptions{
		Level: level,
	}

	if strings.ToLower(cfg.Format) == "json" {
		handler = slog.NewJSONHandler(output, opts)
	} else {
		handler = slog.NewTextHandler(output, opts)
	}

	Logger = slog.New(handler)
	slog.SetDefault(Logger)
	return nil
}

// Close closes the log file if one was opened.
// ToFile reports whether the logger writes to a file (as opposed to
// stdout/stderr captured by a service manager). Fatal startup errors are
// mirrored to stderr in that case so they are visible on the terminal too.
func ToFile() bool {
	return logFile != nil
}

func Close() {
	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Helper functions for common log operations

// Debug logs a debug message with optional key-value pairs.
func Debug(msg string, args ...any) {
	if Logger != nil {
		Logger.Debug(msg, args...)
	}
}

// Info logs an info message with optional key-value pairs.
func Info(msg string, args ...any) {
	if Logger != nil {
		Logger.Info(msg, args...)
	}
}

// Warn logs a warning message with optional key-value pairs.
func Warn(msg string, args ...any) {
	if Logger != nil {
		Logger.Warn(msg, args...)
	}
}

// Error logs an error message with optional key-value pairs.
func Error(msg string, args ...any) {
	if Logger != nil {
		Logger.Error(msg, args...)
	}
}

// With returns a logger with the given attributes added.
func With(args ...any) *slog.Logger {
	if Logger != nil {
		return Logger.With(args...)
	}
	return slog.Default().With(args...)
}
