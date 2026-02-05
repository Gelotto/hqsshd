package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	DefaultSocketPath = "/tmp/hqssh.sock"
	DefaultTCPPort    = 50051 // Default gRPC port, bound to localhost only
	DefaultConfigDir  = ".hqssh"
	DefaultConfigFile = "daemon.yaml"
	DaemonVersion     = "0.1.0"
)

// Config represents the daemon configuration
type Config struct {
	// Socket path for gRPC server (Unix socket)
	Socket string `yaml:"socket"`

	// TCP port for gRPC server (0 = disabled, >0 = listen on localhost:port)
	// Used for SSH tunnel forwarding from mobile clients
	TCPPort int `yaml:"tcp_port"`

	// Session configuration
	Sessions SessionConfig `yaml:"sessions"`

	// Project discovery configuration
	Projects ProjectConfig `yaml:"projects"`

	// Tool configuration
	Tools []ToolConfig `yaml:"tools"`

	// Logging configuration
	Log LogConfig `yaml:"log"`
}

type SessionConfig struct {
	IdleTimeout      int    `yaml:"idle_timeout"`       // Seconds before session considered idle (default: 86400 = 24h)
	MaxSessions      int    `yaml:"max_sessions"`       // Maximum concurrent sessions (default: 20)
	HistorySize      int    `yaml:"history_size"`       // Lines of scrollback (default: 10000)
	LogRetentionDays int    `yaml:"log_retention_days"` // Days to keep ended session logs (default: 30, 0 = forever)
	LogDirectory     string `yaml:"log_directory"`      // Directory for session logs (default: ~/.hqssh/logs/sessions)
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
			IdleTimeout:      86400, // 24 hours
			MaxSessions:      20,
			HistorySize:      10000,
			LogRetentionDays: 30,    // 30 days
			LogDirectory:     "",    // Empty = default (~/.hqssh/logs/sessions)
		},
		Projects: ProjectConfig{
			ScanDirectories: []string{
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

// Load loads configuration from the default location or returns default config
func Load() (*Config, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return DefaultConfig(), nil
	}

	configPath := filepath.Join(homeDir, DefaultConfigDir, DefaultConfigFile)
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
	if err := os.MkdirAll(configDir, 0755); err != nil {
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

	return os.WriteFile(path, data, 0644)
}
