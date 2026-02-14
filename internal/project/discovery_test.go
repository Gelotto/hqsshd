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
	"os"
	"path/filepath"
	"testing"

	"github.com/gelotto/hqsshd/internal/config"
	"github.com/gelotto/hqsshd/internal/tools"
)

func createTestGitRepo(t *testing.T, dir string) {
	t.Helper()

	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatalf("failed to create .git dir: %v", err)
	}
}

func TestNewDiscovery(t *testing.T) {
	cfg := config.DefaultConfig()
	detector := tools.NewDetector(cfg)

	d := NewDiscovery(cfg, detector)

	if d == nil {
		t.Fatal("NewDiscovery returned nil")
	}
	if d.config != cfg {
		t.Error("config not set correctly")
	}
	if d.detector != detector {
		t.Error("detector not set correctly")
	}
}

func TestDiscovery_Discover_EmptyDirectories(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Projects.ScanDirectories = []string{"/nonexistent/path/that/does/not/exist"}
	detector := tools.NewDetector(cfg)
	d := NewDiscovery(cfg, detector)

	projects, scanned, err := d.Discover(nil, 0)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(projects) != 0 {
		t.Errorf("Discover() returned %d projects, want 0", len(projects))
	}
	if scanned != 0 {
		t.Errorf("scanned = %d, want 0", scanned)
	}
}

func TestDiscovery_Discover_FindsGitRepos(t *testing.T) {
	// Create test directory structure
	root := t.TempDir()

	// Create some git repos
	repo1 := filepath.Join(root, "project1")
	repo2 := filepath.Join(root, "project2")
	notRepo := filepath.Join(root, "notgit")

	os.MkdirAll(repo1, 0755)
	os.MkdirAll(repo2, 0755)
	os.MkdirAll(notRepo, 0755)

	createTestGitRepo(t, repo1)
	createTestGitRepo(t, repo2)
	// notRepo doesn't have .git

	cfg := config.DefaultConfig()
	cfg.Projects.ScanDirectories = []string{root}
	cfg.Projects.MaxDepth = 1
	detector := tools.NewDetector(cfg)
	d := NewDiscovery(cfg, detector)

	projects, _, err := d.Discover(nil, 0)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(projects) != 2 {
		t.Errorf("Discover() returned %d projects, want 2", len(projects))
	}
}

func TestDiscovery_Discover_RespectsMaxDepth(t *testing.T) {
	// Create nested directory structure
	root := t.TempDir()

	// Depth 1 repo (root/level1)
	repo1 := filepath.Join(root, "level1")
	os.MkdirAll(repo1, 0755)
	createTestGitRepo(t, repo1)

	// Depth 2 repo (root/parent/level2)
	repo2 := filepath.Join(root, "parent", "level2")
	os.MkdirAll(repo2, 0755)
	createTestGitRepo(t, repo2)

	// Depth 3 repo (root/a/b/level3)
	repo3 := filepath.Join(root, "a", "b", "level3")
	os.MkdirAll(repo3, 0755)
	createTestGitRepo(t, repo3)

	// Depth 4 repo - should be missed with maxDepth=2 (root/x/y/z/level4)
	repo4 := filepath.Join(root, "x", "y", "z", "level4")
	os.MkdirAll(repo4, 0755)
	createTestGitRepo(t, repo4)

	cfg := config.DefaultConfig()
	detector := tools.NewDetector(cfg)
	d := NewDiscovery(cfg, detector)

	// With maxDepth=2, should find repos at depth 1, 2, 3 (but not depth 4)
	// The algorithm visits up to depth 2 directories and iterates their children
	projects, _, err := d.Discover([]string{root}, 2)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(projects) != 3 {
		t.Errorf("Discover(maxDepth=2) returned %d projects, want 3", len(projects))
	}

	// With maxDepth=3, should find all 4
	projects, _, err = d.Discover([]string{root}, 3)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(projects) != 4 {
		t.Errorf("Discover(maxDepth=3) returned %d projects, want 4", len(projects))
	}
}

func TestDiscovery_Discover_SkipsExcluded(t *testing.T) {
	root := t.TempDir()

	// Create repos including one in excluded directory
	normalRepo := filepath.Join(root, "myproject")
	nodeModulesRepo := filepath.Join(root, "node_modules", "somepackage")

	os.MkdirAll(normalRepo, 0755)
	os.MkdirAll(nodeModulesRepo, 0755)

	createTestGitRepo(t, normalRepo)
	createTestGitRepo(t, nodeModulesRepo)

	cfg := config.DefaultConfig()
	cfg.Projects.Exclude = []string{"node_modules"}
	detector := tools.NewDetector(cfg)
	d := NewDiscovery(cfg, detector)

	projects, _, err := d.Discover([]string{root}, 3)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	// Should only find normalRepo
	if len(projects) != 1 {
		t.Errorf("Discover() returned %d projects, want 1 (excluding node_modules)", len(projects))
	}
}

func TestDiscovery_Discover_SkipsHiddenDirs(t *testing.T) {
	root := t.TempDir()

	normalRepo := filepath.Join(root, "normal")
	hiddenRepo := filepath.Join(root, ".hidden", "secret")

	os.MkdirAll(normalRepo, 0755)
	os.MkdirAll(hiddenRepo, 0755)

	createTestGitRepo(t, normalRepo)
	createTestGitRepo(t, hiddenRepo)

	cfg := config.DefaultConfig()
	detector := tools.NewDetector(cfg)
	d := NewDiscovery(cfg, detector)

	projects, _, err := d.Discover([]string{root}, 3)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	// Should only find normalRepo (hidden dirs are skipped)
	if len(projects) != 1 {
		t.Errorf("Discover() returned %d projects, want 1 (excluding hidden)", len(projects))
	}
}

func TestDiscovery_Discover_CustomDirectories(t *testing.T) {
	root := t.TempDir()

	repo := filepath.Join(root, "myrepo")
	os.MkdirAll(repo, 0755)
	createTestGitRepo(t, repo)

	cfg := config.DefaultConfig()
	cfg.Projects.ScanDirectories = []string{"/nonexistent"} // Should be overridden
	detector := tools.NewDetector(cfg)
	d := NewDiscovery(cfg, detector)

	// Pass custom directories
	projects, _, err := d.Discover([]string{root}, 1)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(projects) != 1 {
		t.Errorf("Discover() with custom dirs returned %d, want 1", len(projects))
	}
}

func TestDiscovery_Discover_RootDirectoryWithRepos(t *testing.T) {
	// Simulates ~ being in the scan list: the scan root itself contains
	// git repos as direct children alongside non-repo directories.
	root := t.TempDir()

	repo1 := filepath.Join(root, "myapp")
	repo2 := filepath.Join(root, "dotfiles")
	notRepo := filepath.Join(root, "Downloads")

	os.MkdirAll(repo1, 0755)
	os.MkdirAll(repo2, 0755)
	os.MkdirAll(notRepo, 0755)

	createTestGitRepo(t, repo1)
	createTestGitRepo(t, repo2)

	cfg := config.DefaultConfig()
	cfg.Projects.ScanDirectories = []string{root}
	cfg.Projects.MaxDepth = 1
	detector := tools.NewDetector(cfg)
	d := NewDiscovery(cfg, detector)

	projects, scanned, err := d.Discover(nil, 0)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(projects) != 2 {
		t.Errorf("Discover() returned %d projects, want 2", len(projects))
	}
	if scanned < 2 {
		t.Errorf("scanned = %d, want >= 2", scanned)
	}
}

func TestDiscovery_CreateFromPath(t *testing.T) {
	root := t.TempDir()
	repoPath := filepath.Join(root, "myproject")
	os.MkdirAll(repoPath, 0755)

	cfg := config.DefaultConfig()
	detector := tools.NewDetector(cfg)
	d := NewDiscovery(cfg, detector)

	project, err := d.CreateFromPath(repoPath, "")
	if err != nil {
		t.Fatalf("CreateFromPath() error = %v", err)
	}
	if project == nil {
		t.Fatal("CreateFromPath() returned nil")
	}
	if project.Name != "myproject" {
		t.Errorf("project.Name = %q, want %q", project.Name, "myproject")
	}
	if project.Path != repoPath {
		t.Errorf("project.Path = %q, want %q", project.Path, repoPath)
	}
	if project.ID == "" {
		t.Error("project.ID should not be empty")
	}
}

func TestDiscovery_CreateFromPath_CustomName(t *testing.T) {
	root := t.TempDir()
	repoPath := filepath.Join(root, "myproject")
	os.MkdirAll(repoPath, 0755)

	cfg := config.DefaultConfig()
	detector := tools.NewDetector(cfg)
	d := NewDiscovery(cfg, detector)

	project, err := d.CreateFromPath(repoPath, "Custom Name")
	if err != nil {
		t.Fatalf("CreateFromPath() error = %v", err)
	}
	if project.Name != "Custom Name" {
		t.Errorf("project.Name = %q, want %q", project.Name, "Custom Name")
	}
}

func TestDiscovery_CreateFromPath_NonexistentPath(t *testing.T) {
	cfg := config.DefaultConfig()
	detector := tools.NewDetector(cfg)
	d := NewDiscovery(cfg, detector)

	_, err := d.CreateFromPath("/nonexistent/path", "")
	if err == nil {
		t.Error("CreateFromPath(nonexistent) should return error")
	}
}

func TestDiscovery_CreateFromPath_FilePath(t *testing.T) {
	// Create a file (not directory)
	root := t.TempDir()
	filePath := filepath.Join(root, "somefile.txt")
	os.WriteFile(filePath, []byte("content"), 0644)

	cfg := config.DefaultConfig()
	detector := tools.NewDetector(cfg)
	d := NewDiscovery(cfg, detector)

	_, err := d.CreateFromPath(filePath, "")
	if err == nil {
		t.Error("CreateFromPath(file) should return error")
	}
}

func TestDiscovery_CreateFromPath_HomeTilde(t *testing.T) {
	cfg := config.DefaultConfig()
	detector := tools.NewDetector(cfg)
	d := NewDiscovery(cfg, detector)

	// This test assumes home directory exists
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot get home directory")
	}

	project, err := d.CreateFromPath("~", "")
	if err != nil {
		t.Fatalf("CreateFromPath(~) error = %v", err)
	}
	if project.Path != home {
		t.Errorf("project.Path = %q, want %q", project.Path, home)
	}
}

func TestGenerateProjectID(t *testing.T) {
	// Same path should give same ID
	id1 := generateProjectID("/path/to/project")
	id2 := generateProjectID("/path/to/project")
	if id1 != id2 {
		t.Errorf("same path should give same ID: %s != %s", id1, id2)
	}

	// Different paths should give different IDs
	id3 := generateProjectID("/different/path")
	if id1 == id3 {
		t.Errorf("different paths should give different IDs")
	}

	// ID should be 16 hex chars (8 bytes)
	if len(id1) != 16 {
		t.Errorf("ID length = %d, want 16", len(id1))
	}
}

func TestDiscovery_ProjectFields(t *testing.T) {
	root := t.TempDir()
	repoPath := filepath.Join(root, "testproject")
	os.MkdirAll(repoPath, 0755)
	createTestGitRepo(t, repoPath)

	cfg := config.DefaultConfig()
	detector := tools.NewDetector(cfg)
	d := NewDiscovery(cfg, detector)

	projects, _, err := d.Discover([]string{root}, 1)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(projects))
	}

	project := projects[0]

	if project.ID == "" {
		t.Error("project.ID should not be empty")
	}
	if project.Name != "testproject" {
		t.Errorf("project.Name = %q, want %q", project.Name, "testproject")
	}
	if project.Path != repoPath {
		t.Errorf("project.Path = %q, want %q", project.Path, repoPath)
	}
	// DetectedTools can be empty depending on system
	if project.DetectedTools == nil {
		t.Error("project.DetectedTools should not be nil")
	}
	if project.IsFavorite {
		t.Error("project.IsFavorite should be false initially")
	}
}
