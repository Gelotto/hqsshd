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

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/gelotto/hqsshd/internal/fsutil"
)

// validToolName matches safe tool names: alphanumeric, hyphens, underscores only.
var validToolName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

const (
	DefaultSocketPath = "/tmp/hqssh.sock"
	DefaultTCPPort    = 50051 // Default gRPC port, bound to localhost only
	DefaultConfigDir  = ".hqssh"
	DefaultConfigFile = "daemon.yaml"
)

// DaemonVersion is the version of the daemon. Overridden by ldflags at build time.
var DaemonVersion = "0.1.0-dev"

// Commit is the git commit SHA. Overridden by ldflags at build time.
var Commit = "unknown"

// Config represents the daemon configuration
type Config struct {
	// Socket path for gRPC server (Unix socket)
	Socket string `yaml:"socket"`

	// TCP port for gRPC server (0 = disabled, >0 = listen on localhost:port)
	// Used for SSH tunnel forwarding from mobile clients
	TCPPort int `yaml:"tcp_port"`

	// Auth token for gRPC authentication (empty = no auth required).
	// When set, clients must pass this token in the "authorization" gRPC metadata header.
	AuthToken string `yaml:"auth_token"`

	// Enable gRPC reflection (default: false). Only enable for debugging with grpcurl.
	EnableReflection bool `yaml:"enable_reflection"`

	// Enable the "shell" tool for raw command execution (default: false).
	// WARNING: The shell tool runs arbitrary commands. Only enable on trusted systems.
	EnableShellTool bool `yaml:"enable_shell_tool"`

	// Session configuration
	Sessions SessionConfig `yaml:"sessions"`

	// Project discovery configuration
	Projects ProjectConfig `yaml:"projects"`

	// Tool configuration
	Tools []ToolConfig `yaml:"tools"`

	// Task configuration
	Tasks TaskConfig `yaml:"tasks"`

	// Logging configuration
	Log LogConfig `yaml:"log"`

	// Event notification configuration
	Events EventsConfig `yaml:"events"`
}

// EventsConfig controls push delivery of session events (bell, ended).
type EventsConfig struct {
	// WebhookURL receives an HTTP POST for every session event. The body is
	// plain text ("<session> needs attention" / "<session> ended") with
	// ntfy-compatible Title/Priority/Tags headers, so pointing this at an
	// https://ntfy.sh/<topic> URL delivers push notifications to a phone
	// even when the HQSSH app is not running. Empty = disabled.
	WebhookURL string `yaml:"webhook_url"`
}

type SessionConfig struct {
	IdleTimeout      int    `yaml:"idle_timeout"`        // Seconds before session considered idle (default: 86400 = 24h)
	MaxSessions      int    `yaml:"max_sessions"`        // Maximum concurrent sessions (default: 20)
	HistorySize      int    `yaml:"history_size"`        // Lines of scrollback (default: 10000)
	LogRetentionDays int    `yaml:"log_retention_days"`  // Days to keep ended session logs (default: 30, 0 = forever)
	LogDirectory     string `yaml:"log_directory"`       // Directory for session logs (default: ~/.hqssh/logs/sessions)
	ClientBufferSize int    `yaml:"client_buffer_size"`  // Per-client output channel buffer, in PTY chunks (default: 1024)
	MaxScrollbackSize int   `yaml:"max_scrollback_size"` // Max scrollback buffer in bytes (default: 10MB)
}

type TaskConfig struct {
	MaxOutputSize  int `yaml:"max_output_size"`    // Max task output in bytes (default: 1MB)
	MaxRunsPerTask int `yaml:"max_runs_per_task"`  // Max retained runs per task (default: 10)
	MaxTimeout     int `yaml:"max_timeout"`        // Maximum allowed timeout in seconds (default: 3600 = 1h, 0 = no limit)
}

type ProjectConfig struct {
	ScanDirectories []string `yaml:"scan_directories"`
	MaxDepth        int      `yaml:"max_depth"`
	Exclude         []string `yaml:"exclude"`
}

type ToolConfig struct {
	Name    string `yaml:"name"`
	Command string `yaml:"command"`
	Detect  string `yaml:"detect"` // Command to detect if tool is installed
}

type LogConfig struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // text, json (default: text)
	File   string `yaml:"file"`   // Log file path (empty = stdout)
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	homeDir, _ := os.UserHomeDir()
	return &Config{
		Socket:  DefaultSocketPath,
		TCPPort: DefaultTCPPort,
		Sessions: SessionConfig{
			IdleTimeout:       86400, // 24 hours
			MaxSessions:       20,
			HistorySize:       10000,
			LogRetentionDays:  30,               // 30 days
			LogDirectory:      "",               // Empty = default (~/.hqssh/logs/sessions)
			ClientBufferSize:  1024,             // Per-client output channel buffer (chunks)
			MaxScrollbackSize: 10 * 1024 * 1024, // 10MB
		},
		Tasks: TaskConfig{
			MaxOutputSize:  1024 * 1024, // 1MB
			MaxRunsPerTask: 10,
			MaxTimeout:     3600, // 1 hour ceiling
		},
		Projects: ProjectConfig{
			ScanDirectories: []string{
				homeDir,
				filepath.Join(homeDir, "projects"),
				filepath.Join(homeDir, "code"),
				filepath.Join(homeDir, "repos"),
				filepath.Join(homeDir, "src"),
				filepath.Join(homeDir, "work"),
			},
			MaxDepth: 3,
			Exclude: []string{
				"node_modules",
				".git",
				"vendor",
				"target",
				"build",
			},
		},
		Tools: []ToolConfig{
			{Name: "claude", Command: "claude", Detect: "which claude"},
			{Name: "codex", Command: "codex", Detect: "which codex"},
			{Name: "aider", Command: "aider", Detect: "which aider"},
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
			File:   "", // Empty = stdout (compatible with systemd journald)
		},
	}
}

// DataDirFor returns the daemon data directory under a home directory.
func DataDirFor(homeDir string) string {
	return filepath.Join(homeDir, DefaultConfigDir)
}

// DataDir returns the daemon data directory (~/.hqssh): config, stores,
// pidfile and logs all live here.
func DataDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return DataDirFor(homeDir), nil
}

// DefaultConfigPath returns ~/.hqssh/daemon.yaml.
func DefaultConfigPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, DefaultConfigFile), nil
}

// Load loads configuration from the default location or returns default config
func Load() (*Config, error) {
	configPath, err := DefaultConfigPath()
	if err != nil {
		return DefaultConfig(), nil
	}
	return LoadFromPath(configPath)
}

// LoadFromPath loads configuration from a specific path
func LoadFromPath(path string) (*Config, error) {
	config := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return config, nil
		}
		return nil, err
	}

	if err := yaml.Unmarshal(data, config); err != nil {
		return nil, err
	}

	return config, nil
}

// Save saves the configuration to the default location
func (c *Config) Save() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	configDir := filepath.Join(homeDir, DefaultConfigDir)
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}

	configPath := filepath.Join(configDir, DefaultConfigFile)
	return c.SaveToPath(configPath)
}

// SaveToPath saves the configuration to a specific path
func (c *Config) SaveToPath(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}

	// Atomic: a crash mid-write must not leave a truncated daemon.yaml that
	// the daemon then rejects with exit 2 on every restart.
	return fsutil.WriteFileAtomic(path, data, 0600)
}

// Validate checks the configuration for security issues and invalid values.
func (c *Config) Validate() error {
	for _, tool := range c.Tools {
		if !validToolName.MatchString(tool.Name) {
			return fmt.Errorf("invalid tool name %q: must match [a-zA-Z0-9_-]+", tool.Name)
		}
		if tool.Command != "" && !validToolName.MatchString(tool.Command) {
			return fmt.Errorf("invalid tool command %q: must match [a-zA-Z0-9_-]+", tool.Command)
		}
	}
	if c.Tasks.MaxTimeout < 0 {
		return fmt.Errorf("tasks.max_timeout must be >= 0")
	}
	return nil
}

// ValidateToolName checks if a tool name contains only safe characters.
func ValidateToolName(name string) bool {
	return validToolName.MatchString(name)
}

// CheckFilePermissions warns if a config file is readable by group or others.
func CheckFilePermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil // File doesn't exist, no problem
	}
	mode := info.Mode().Perm()
	if mode&0077 != 0 {
		return fmt.Errorf("config file %s has permissions %04o; should be 0600 (run: chmod 600 %s)", path, mode, path)
	}
	return nil
}
