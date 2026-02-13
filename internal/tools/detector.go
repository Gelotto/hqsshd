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
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/gelotto/hqsshd/internal/config"
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

	var installed []string
	for _, tool := range d.config.Tools {
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

// GetCommand returns the command for a tool
func (d *Detector) GetCommand(name string) string {
	for _, t := range d.config.Tools {
		if t.Name == name {
			return t.Command
		}
	}
	return name // Fallback to tool name as command
}

// ClearCache clears the detection cache
func (d *Detector) ClearCache() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cache = make(map[string]bool)
}

// detectTool runs the detection command for a tool.
// Commands are wrapped in a login shell to ensure tools installed via
// nvm/pyenv/asdf/cargo are available on PATH.
func (d *Detector) detectTool(tool config.ToolConfig) bool {
	if tool.Detect == "" {
		// Default: try "which <command>"
		tool.Detect = "which " + tool.Command
	}

	if strings.TrimSpace(tool.Detect) == "" {
		return false
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}

	cmd := exec.Command(shell, "-l", "-c", tool.Detect)
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
