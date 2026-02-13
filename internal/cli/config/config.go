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
	"strconv"

	"gopkg.in/yaml.v3"
)

// Config holds CLI configuration loaded from file and environment.
type Config struct {
	DefaultHost string          `yaml:"default_host"`
	Hosts       map[string]Host `yaml:"hosts"`
}

// Host holds connection settings for a named host.
type Host struct {
	Host     string `yaml:"host"`     // Actual hostname (optional, defaults to key name)
	User     string `yaml:"user"`
	Port     int    `yaml:"port"`
	Key      string `yaml:"key"`
	Password string `yaml:"password"` // Not recommended
}

// Resolved holds the final resolved connection settings after
// applying CLI flags > env vars > config file > defaults.
type Resolved struct {
	Host     string
	User     string
	Port     int
	Key      string
	Password string
}

var globalConfig *Config

// Load reads the config file from ~/.hqssh/config.yaml.
// Returns empty config if file doesn't exist.
// Warns if the config file has overly permissive permissions.
func Load() *Config {
	if globalConfig != nil {
		return globalConfig
	}

	globalConfig = &Config{
		Hosts: make(map[string]Host),
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return globalConfig
	}

	configPath := filepath.Join(home, ".hqssh", "config.yaml")

	// Check file permissions before reading (may contain passwords)
	if info, statErr := os.Stat(configPath); statErr == nil {
		mode := info.Mode().Perm()
		if mode&0077 != 0 {
			fmt.Fprintf(os.Stderr, "Warning: %s has permissions %04o (should be 0600)\n", configPath, mode)
			fmt.Fprintf(os.Stderr, "  Fix with: chmod 600 %s\n", configPath)
		}
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return globalConfig
	}

	if err := yaml.Unmarshal(data, globalConfig); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to parse %s: %v\n", configPath, err)
	}
	return globalConfig
}

// Resolve computes final connection settings from all sources.
// Priority: CLI flag > environment variable > config file > default
func Resolve(cliHost, cliUser, cliKey, cliPassword string, cliPort int) Resolved {
	cfg := Load()
	r := Resolved{
		Port: 22,
		User: os.Getenv("USER"),
	}

	// Start with config file defaults
	if cfg.DefaultHost != "" {
		hostCfg, ok := cfg.Hosts[cfg.DefaultHost]
		if ok {
			if hostCfg.Host != "" {
				r.Host = hostCfg.Host
			} else {
				r.Host = cfg.DefaultHost
			}
			if hostCfg.User != "" {
				r.User = hostCfg.User
			}
			if hostCfg.Port != 0 {
				r.Port = hostCfg.Port
			}
			if hostCfg.Key != "" {
				r.Key = expandPath(hostCfg.Key)
			}
			if hostCfg.Password != "" {
				r.Password = hostCfg.Password
			}
		} else {
			r.Host = cfg.DefaultHost
		}
	}

	// Environment variables override config file
	if env := os.Getenv("HQSSH_HOST"); env != "" {
		r.Host = env
	}
	if env := os.Getenv("HQSSH_USER"); env != "" {
		r.User = env
	}
	if env := os.Getenv("HQSSH_PORT"); env != "" {
		if p, err := strconv.Atoi(env); err == nil {
			r.Port = p
		}
	}
	if env := os.Getenv("HQSSH_KEY"); env != "" {
		r.Key = expandPath(env)
	}

	// CLI flags override everything
	if cliHost != "" {
		// Check if it's a host alias
		if hostCfg, ok := cfg.Hosts[cliHost]; ok {
			if hostCfg.Host != "" {
				r.Host = hostCfg.Host
			} else {
				r.Host = cliHost
			}
			// Apply host-specific settings unless overridden by CLI
			if cliUser == "" && hostCfg.User != "" {
				r.User = hostCfg.User
			}
			if cliPort == 0 && hostCfg.Port != 0 {
				r.Port = hostCfg.Port
			}
			if cliKey == "" && hostCfg.Key != "" {
				r.Key = expandPath(hostCfg.Key)
			}
			if cliPassword == "" && hostCfg.Password != "" {
				r.Password = hostCfg.Password
			}
		} else {
			r.Host = cliHost
		}
	}
	if cliUser != "" {
		r.User = cliUser
	}
	if cliPort != 0 {
		r.Port = cliPort
	}
	if cliKey != "" {
		r.Key = expandPath(cliKey)
	}
	if cliPassword != "" {
		r.Password = cliPassword
	}

	return r
}

// expandPath expands ~ to home directory
func expandPath(path string) string {
	if len(path) > 0 && path[0] == '~' {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

// GetConfigPath returns the path to the config file.
func GetConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".hqssh", "config.yaml")
}
