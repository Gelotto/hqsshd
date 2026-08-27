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
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gelotto/hqsshd/internal/config"
	"github.com/gelotto/hqsshd/internal/logging"
	"github.com/gelotto/hqsshd/internal/shellutil"
)

const (
	// DefaultDetectTimeout bounds one tool lookup. Lookups that miss the
	// fast path run a login shell, and a slow or prompting ~/.zprofile used
	// to hang every GetInfo (the app's connection check) indefinitely.
	DefaultDetectTimeout = 5 * time.Second
	// DefaultCacheTTL is how long a full detection result is reused.
	DefaultCacheTTL = 60 * time.Second
)

// Detector detects installed AI CLI tools
type Detector struct {
	config  *config.Config
	timeout time.Duration
	ttl     time.Duration

	mu       sync.Mutex
	cache    map[string]bool
	cachedAt time.Time     // zero until the first full detection
	refresh  chan struct{} // non-nil while a full detection runs; closed when done
}

// NewDetector creates a new tool detector
func NewDetector(cfg *config.Config) *Detector {
	return &Detector{
		config:  cfg,
		timeout: DefaultDetectTimeout,
		ttl:     DefaultCacheTTL,
		cache:   make(map[string]bool),
	}
}

// DetectAll returns the configured tools that are installed, in config
// order. Results are cached for the TTL. An expired cache is served as-is
// while one background refresh runs (stale-while-revalidate), so a caller
// on the GetInfo hot path never waits for login-shell probes; only the very
// first call (normally Warm at startup) probes synchronously.
func (d *Detector) DetectAll() []string {
	d.mu.Lock()
	if !d.cachedAt.IsZero() {
		if time.Since(d.cachedAt) >= d.ttl && d.refresh == nil {
			done := make(chan struct{})
			d.refresh = done
			go d.refreshAll(done)
		}
		result := d.installedLocked()
		d.mu.Unlock()
		return result
	}
	if d.refresh != nil {
		// A cold-cache refresh is already running; share it
		done := d.refresh
		d.mu.Unlock()
		<-done
		d.mu.Lock()
		result := d.installedLocked()
		d.mu.Unlock()
		return result
	}
	done := make(chan struct{})
	d.refresh = done
	d.mu.Unlock()

	d.refreshAll(done)

	d.mu.Lock()
	result := d.installedLocked()
	d.mu.Unlock()
	return result
}

// refreshAll probes every tool and installs the result, then closes done.
func (d *Detector) refreshAll(done chan struct{}) {
	results := d.detectAllUncached()
	d.mu.Lock()
	d.cache = results
	d.cachedAt = time.Now()
	d.refresh = nil
	d.mu.Unlock()
	close(done)
}

// Warm runs a full detection so later calls hit the cache.
func (d *Detector) Warm() {
	d.DetectAll()
}

// installedLocked lists cached-installed tools in config order. Caller
// holds d.mu.
func (d *Detector) installedLocked() []string {
	installed := make([]string, 0, len(d.config.Tools))
	for _, tool := range d.config.Tools {
		if d.cache[tool.Name] {
			installed = append(installed, tool.Name)
		}
	}
	return installed
}

// detectAllUncached probes every configured tool concurrently. Runs
// without holding d.mu.
func (d *Detector) detectAllUncached() map[string]bool {
	type result struct {
		name string
		ok   bool
	}
	resultCh := make(chan result, len(d.config.Tools))
	var wg sync.WaitGroup
	for _, tool := range d.config.Tools {
		if !config.ValidateToolName(tool.Name) {
			logging.Warn("skipping tool with invalid name", "name", tool.Name)
			continue
		}
		wg.Add(1)
		go func(tool config.ToolConfig) {
			defer wg.Done()
			resultCh <- result{name: tool.Name, ok: d.detectTool(tool)}
		}(tool)
	}
	wg.Wait()
	close(resultCh)

	results := make(map[string]bool, len(d.config.Tools))
	for r := range resultCh {
		results[r.name] = r.ok
	}
	return results
}

// IsInstalled checks if a specific tool is installed
func (d *Detector) IsInstalled(name string) bool {
	d.mu.Lock()
	if cached, ok := d.cache[name]; ok {
		d.mu.Unlock()
		return cached
	}
	d.mu.Unlock()

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

	result := d.detectTool(*toolCfg)

	d.mu.Lock()
	d.cache[name] = result
	d.mu.Unlock()
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
	d.cachedAt = time.Time{}
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
		if _, err := exec.LookPath(tool.Command); err == nil {
			return true
		}
		// Fallback: try via login shell for tools installed by nvm/pyenv/asdf
		// that modify PATH in shell init scripts
		if !config.ValidateToolName(tool.Command) {
			return false
		}
		// "command -v" is a POSIX builtin, safer than "which"
		return d.runInLoginShell(tool.Name, "command -v "+tool.Command)
	}

	if strings.TrimSpace(tool.Detect) == "" {
		return false
	}
	return d.runInLoginShell(tool.Name, tool.Detect)
}

// runInLoginShell runs script in the user's login shell with a timeout and
// reports whether it exited 0. The shell gets its own process group so a
// timeout kills anything it spawned, not just the shell.
func (d *Detector) runInLoginShell(toolName, script string) bool {
	shell := shellutil.UserShell()

	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, shell, "-l", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 500 * time.Millisecond

	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		logging.Warn("tool detection timed out",
			"tool", toolName,
			"timeout", d.timeout,
			"shell", shell,
		)
		return false
	}
	return err == nil
}

// DetectForProject detects tools available in a specific project directory
// This can be extended to detect project-specific tools (e.g., from package.json)
func (d *Detector) DetectForProject(projectPath string) []string {
	// For now, just return system-wide installed tools
	// TODO: Add project-specific detection (e.g., npm scripts, Makefile targets)
	return d.DetectAll()
}
