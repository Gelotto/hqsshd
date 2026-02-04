package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const registryFile = "projects.json"

// Registry stores and manages projects
type Registry struct {
	projects map[string]*Project
	mu       sync.RWMutex
	dataDir  string
}

// NewRegistry creates a new project registry
func NewRegistry(dataDir string) *Registry {
	return &Registry{
		projects: make(map[string]*Project),
		dataDir:  dataDir,
	}
}

// Load loads the registry from disk
func (r *Registry) Load() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	path := filepath.Join(r.dataDir, registryFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Empty registry is fine
		}
		return err
	}

	var projects []*Project
	if err := json.Unmarshal(data, &projects); err != nil {
		return err
	}

	r.projects = make(map[string]*Project)
	for _, p := range projects {
		r.projects[p.ID] = p
	}

	return nil
}

// Save persists the registry to disk
func (r *Registry) Save() error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Ensure data directory exists
	if err := os.MkdirAll(r.dataDir, 0755); err != nil {
		return err
	}

	projects := make([]*Project, 0, len(r.projects))
	for _, p := range r.projects {
		projects = append(projects, p)
	}

	data, err := json.MarshalIndent(projects, "", "  ")
	if err != nil {
		return err
	}

	path := filepath.Join(r.dataDir, registryFile)
	return os.WriteFile(path, data, 0644)
}

// Add adds a project to the registry
func (r *Registry) Add(project *Project) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.projects[project.ID] = project
}

// Remove removes a project from the registry
func (r *Registry) Remove(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.projects[id]; exists {
		delete(r.projects, id)
		return true
	}
	return false
}

// Get retrieves a project by ID
func (r *Registry) Get(id string) *Project {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.projects[id]
}

// GetByPath retrieves a project by its path
func (r *Registry) GetByPath(path string) *Project {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, p := range r.projects {
		if p.Path == path {
			return p
		}
	}
	return nil
}

// List returns all projects
func (r *Registry) List(favoritesOnly bool) []*Project {
	r.mu.RLock()
	defer r.mu.RUnlock()

	projects := make([]*Project, 0, len(r.projects))
	for _, p := range r.projects {
		if favoritesOnly && !p.IsFavorite {
			continue
		}
		projects = append(projects, p)
	}
	return projects
}

// UpdateLastAccessed updates the last accessed time for a project
func (r *Registry) UpdateLastAccessed(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if p, exists := r.projects[id]; exists {
		p.LastAccessed = time.Now()
	}
}

// SetFavorite sets the favorite status of a project
func (r *Registry) SetFavorite(id string, favorite bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if p, exists := r.projects[id]; exists {
		p.IsFavorite = favorite
		return true
	}
	return false
}

// MergeDiscovered merges discovered projects with existing registry
// Only adds new projects, preserves existing project metadata
func (r *Registry) MergeDiscovered(discovered []*Project) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	added := 0
	for _, p := range discovered {
		if _, exists := r.projects[p.ID]; !exists {
			r.projects[p.ID] = p
			added++
		}
	}
	return added
}

// Count returns the number of registered projects
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.projects)
}
