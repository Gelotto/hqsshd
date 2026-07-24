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

package tools

import (
	"os/exec"
	"strings"
	"sync"

	"github.com/gelotto/hqsshd/internal/config"
	"github.com/gelotto/hqsshd/internal/logging"
	"github.com/gelotto/hqsshd/internal/shellutil"
)

// Detector detects installed AI CLI tools
type Detector struct {
	config *config.Config
	cache  map[string]bool
	mu     sync.RWMutex
}

// NewDetector creates a new tool detector
func NewDetector(cfg *config.Config) *Detector {
	return &Detector{
		config: cfg,
		cache:  make(map[string]bool),
	}
}

// DetectAll detects all configured tools and returns list of installed ones
func (d *Detector) DetectAll() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	installed := make([]string, 0)
	for _, tool := range d.config.Tools {
		if !config.ValidateToolName(tool.Name) {
			logging.Warn("skipping tool with invalid name", "name", tool.Name)
			continue
		}
		if d.detectTool(tool) {
			installed = append(installed, tool.Name)
			d.cache[tool.Name] = true
		} else {
			d.cache[tool.Name] = false
		}
	}
	return installed
}

// IsInstalled checks if a specific tool is installed
func (d *Detector) IsInstalled(name string) bool {
	d.mu.RLock()
	if cached, ok := d.cache[name]; ok {
		d.mu.RUnlock()
		return cached
	}
	d.mu.RUnlock()

	// Find tool config
	var toolCfg *config.ToolConfig
	for _, t := range d.config.Tools {
		if t.Name == name {
			toolCfg = &t
			break
		}
	}
	if toolCfg == nil {
		return false
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	result := d.detectTool(*toolCfg)
	d.cache[name] = result
	return result
}

// GetCommand returns the command for a tool.
// Returns empty string if the tool is not in the configured list.
func (d *Detector) GetCommand(name string) string {
	for _, t := range d.config.Tools {
		if t.Name == name {
			return t.Command
		}
	}
	return "" // Don't echo untrusted input back as a command
}

// ClearCache clears the detection cache
func (d *Detector) ClearCache() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cache = make(map[string]bool)
}

// detectTool detects if a tool is installed.
// When no custom Detect command is configured, uses exec.LookPath() which is
// safe and doesn't invoke a shell. Custom Detect commands are wrapped in a
// login shell to ensure tools installed via nvm/pyenv/asdf/cargo are available.
func (d *Detector) detectTool(tool config.ToolConfig) bool {
	// Validate tool name to prevent injection
	if !config.ValidateToolName(tool.Name) {
		return false
	}

	if tool.Detect == "" {
		// Safe default: use exec.LookPath instead of shelling out to "which"
		_, err := exec.LookPath(tool.Command)
		if err == nil {
			return true
		}
		// Fallback: try via login shell for tools installed by nvm/pyenv/asdf
		// that modify PATH in shell init scripts
		return d.detectViaLoginShell(tool.Command)
	}

	if strings.TrimSpace(tool.Detect) == "" {
		return false
	}

	shell := shellutil.UserShell()

	cmd := exec.Command(shell, "-l", "-c", tool.Detect)
	err := cmd.Run()
	return err == nil
}

// detectViaLoginShell tries to find a tool by running "command -v" in a login shell.
// This catches tools installed via version managers (nvm, pyenv, asdf) that modify
// PATH in shell init scripts.
func (d *Detector) detectViaLoginShell(command string) bool {
	if !config.ValidateToolName(command) {
		return false
	}

	shell := shellutil.UserShell()

	// Use "command -v" (POSIX builtin, safer than "which")
	cmd := exec.Command(shell, "-l", "-c", "command -v "+command)
	err := cmd.Run()
	return err == nil
}

// DetectForProject detects tools available in a specific project directory
// This can be extended to detect project-specific tools (e.g., from package.json)
func (d *Detector) DetectForProject(projectPath string) []string {
	// For now, just return system-wide installed tools
	// TODO: Add project-specific detection (e.g., npm scripts, Makefile targets)
	return d.DetectAll()
}
