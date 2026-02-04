// Package logging provides structured logging for the hqsshd daemon.
package logging

import (
	"io"
	"log/slog"
	"os"
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

	// If file is specified, open it for logging
	if cfg.File != "" {
		f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return err
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
