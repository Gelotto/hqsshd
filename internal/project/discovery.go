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

package project

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gelotto/hqsshd/internal/config"
	"github.com/gelotto/hqsshd/internal/tools"
)

// Project represents a discovered or registered project
type Project struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Path          string    `json:"path"`
	DetectedTools []string  `json:"detected_tools"`
	LastAccessed  time.Time `json:"last_accessed"`
	IsFavorite    bool      `json:"is_favorite"`
}

// Discovery handles project discovery and management
type Discovery struct {
	config   *config.Config
	detector *tools.Detector
}

// NewDiscovery creates a new project discovery instance
func NewDiscovery(cfg *config.Config, detector *tools.Detector) *Discovery {
	return &Discovery{
		config:   cfg,
		detector: detector,
	}
}

// Discover scans the configured directories for git repositories
func (d *Discovery) Discover(directories []string, maxDepth int) ([]*Project, int, error) {
	if len(directories) == 0 {
		directories = d.config.Projects.ScanDirectories
	}
	if maxDepth <= 0 {
		maxDepth = d.config.Projects.MaxDepth
	}

	var projects []*Project
	totalScanned := 0

	for _, dir := range directories {
		dir = expandHome(dir)

		// Check if directory exists
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}

		discovered, scanned := d.scanDirectory(dir, maxDepth)
		projects = append(projects, discovered...)
		totalScanned += scanned
	}

	return projects, totalScanned, nil
}

// scanDirectory recursively scans a directory for git repos
func (d *Discovery) scanDirectory(root string, maxDepth int) ([]*Project, int) {
	var projects []*Project
	scanned := 0

	d.walkDirectory(root, 0, maxDepth, func(path string) {
		scanned++

		// Check if this is a git repo
		gitPath := filepath.Join(path, ".git")
		if info, err := os.Stat(gitPath); err == nil && info.IsDir() {
			project := d.createProject(path)
			projects = append(projects, project)
		}
	})

	return projects, scanned
}

// walkDirectory walks a directory up to maxDepth
func (d *Discovery) walkDirectory(dir string, depth, maxDepth int, callback func(string)) {
	if depth > maxDepth {
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()

		// Skip hidden directories (except .git check happens after)
		if strings.HasPrefix(name, ".") {
			continue
		}

		// Skip excluded directories
		if d.isExcluded(name) {
			continue
		}

		fullPath := filepath.Join(dir, name)
		callback(fullPath)

		// Continue walking if we haven't hit max depth
		if depth < maxDepth {
			d.walkDirectory(fullPath, depth+1, maxDepth, callback)
		}
	}
}

// isExcluded checks if a directory name is in the exclude list
func (d *Discovery) isExcluded(name string) bool {
	for _, excluded := range d.config.Projects.Exclude {
		if name == excluded {
			return true
		}
	}
	return false
}

// createProject creates a Project from a directory path
func (d *Discovery) createProject(path string) *Project {
	name := filepath.Base(path)
	id := generateProjectID(path)

	// Detect tools for this project
	detectedTools := d.detector.DetectForProject(path)

	return &Project{
		ID:            id,
		Name:          name,
		Path:          path,
		DetectedTools: detectedTools,
		LastAccessed:  time.Time{}, // Not accessed yet
		IsFavorite:    false,
	}
}

// CreateFromPath creates a project from a specific path
func (d *Discovery) CreateFromPath(path, name string) (*Project, error) {
	path = expandHome(path)

	// Verify path exists
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, os.ErrNotExist
	}

	if name == "" {
		name = filepath.Base(path)
	}

	project := &Project{
		ID:            generateProjectID(path),
		Name:          name,
		Path:          path,
		DetectedTools: d.detector.DetectForProject(path),
		LastAccessed:  time.Now(),
		IsFavorite:    false,
	}

	return project, nil
}

// expandHome expands a leading ~ to the user's home directory
func expandHome(path string) string {
	if strings.HasPrefix(path, "~") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[1:])
	}
	return path
}

// generateProjectID generates a unique ID for a project based on its path
func generateProjectID(path string) string {
	hash := sha256.Sum256([]byte(path))
	return hex.EncodeToString(hash[:8]) // First 16 hex chars
}
