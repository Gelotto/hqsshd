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

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gelotto/hqsshd/internal/cli/client"
	pb "github.com/gelotto/hqsshd/proto"
	"github.com/spf13/cobra"
)

var (
	projectsJSON bool
)

var projectsCmd = &cobra.Command{
	Use:   "projects",
	Short: "List registered projects",
	Long: `List all registered projects on the remote system.

Projects are git repositories that the daemon tracks for AI tool sessions.

Examples:
  hqssh projects                       # List all projects
  hqssh projects --json                # Machine-readable output
  hqssh projects add /path/to/repo     # Register a project
  hqssh projects remove <id>           # Unregister a project
  hqssh projects discover              # Scan for git repos
  hqssh projects tools <id>            # Show detected AI tools`,
	RunE: runProjects,
}

var projectAddCmd = &cobra.Command{
	Use:   "add <path>",
	Short: "Register a project directory",
	Args:  cobra.ExactArgs(1),
	RunE:  runProjectAdd,
}

var projectRemoveCmd = &cobra.Command{
	Use:   "remove <id>",
	Short: "Unregister a project",
	Args:  cobra.ExactArgs(1),
	RunE:  runProjectRemove,
}

var projectDiscoverCmd = &cobra.Command{
	Use:   "discover [dirs...]",
	Short: "Scan directories for git repos",
	Long: `Scan directories for git repositories and register them as projects.

If no directories are specified, uses the daemon's default scan directories.

Examples:
  hqssh projects discover                    # Use default scan dirs
  hqssh projects discover ~/code ~/work      # Scan specific directories`,
	RunE: runProjectDiscover,
}

var projectToolsCmd = &cobra.Command{
	Use:   "tools <id>",
	Short: "Show detected AI tools for a project",
	Args:  cobra.ExactArgs(1),
	RunE:  runProjectTools,
}

func init() {
	projectsCmd.Flags().BoolVar(&projectsJSON, "json", false, "Output as JSON")
	projectsCmd.AddCommand(projectAddCmd)
	projectsCmd.AddCommand(projectRemoveCmd)
	projectsCmd.AddCommand(projectDiscoverCmd)
	projectsCmd.AddCommand(projectToolsCmd)
	rootCmd.AddCommand(projectsCmd)
}

func runProjects(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, hostLabel, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := c.ProjectService.List(ctx, &pb.ListProjectsRequest{})
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}

	if projectsJSON {
		type jsonProject struct {
			ID           string   `json:"id"`
			Name         string   `json:"name"`
			Path         string   `json:"path"`
			Tools        []string `json:"detected_tools,omitempty"`
			LastAccessed int64    `json:"last_accessed,omitempty"`
			IsFavorite   bool     `json:"is_favorite,omitempty"`
		}
		out := make([]jsonProject, len(resp.Projects))
		for i, p := range resp.Projects {
			out[i] = jsonProject{
				ID:           p.Id,
				Name:         p.Name,
				Path:         p.Path,
				Tools:        p.DetectedTools,
				LastAccessed: p.LastAccessed,
				IsFavorite:   p.IsFavorite,
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(resp.Projects) == 0 {
		fmt.Println("No projects registered.")
		fmt.Println("\nTo register a project:")
		fmt.Printf("  hqssh projects add /path/to/repo%s\n", hostFlag(hostLabel))
		fmt.Println("\nTo discover projects automatically:")
		fmt.Printf("  hqssh projects discover%s\n", hostFlag(hostLabel))
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tPATH\tTOOLS\tLAST ACCESSED")
	fmt.Fprintln(w, "--\t----\t----\t-----\t-------------")

	for _, p := range resp.Projects {
		tools := "-"
		if len(p.DetectedTools) > 0 {
			tools = strings.Join(p.DetectedTools, ", ")
		}

		lastAccessed := "Never"
		if p.LastAccessed > 0 {
			lastAccessed = formatTime(p.LastAccessed)
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			shortID(p.Id),
			truncate(p.Name, 20),
			truncate(p.Path, 40),
			tools,
			lastAccessed,
		)
	}
	w.Flush()

	return nil
}

func runProjectAdd(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, _, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	proj, err := c.ProjectService.Add(ctx, &pb.AddProjectRequest{
		Path: args[0],
	})
	if err != nil {
		return fmt.Errorf("add project: %w", err)
	}

	fmt.Printf("Project registered: %s (%s)\n", proj.Name, shortID(proj.Id))
	fmt.Printf("  Path: %s\n", proj.Path)
	if len(proj.DetectedTools) > 0 {
		fmt.Printf("  Tools: %s\n", strings.Join(proj.DetectedTools, ", "))
	}

	return nil
}

func runProjectRemove(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, _, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	projectID, err := resolveProjectByPrefix(ctx, c, args[0])
	if err != nil {
		return err
	}

	_, err = c.ProjectService.Remove(ctx, &pb.RemoveProjectRequest{
		ProjectId: projectID,
	})
	if err != nil {
		return fmt.Errorf("remove project: %w", err)
	}

	fmt.Printf("Project removed: %s\n", shortID(projectID))
	return nil
}

func runProjectDiscover(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, _, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	fmt.Fprintf(os.Stderr, "Scanning for projects...\n")

	resp, err := c.ProjectService.Discover(ctx, &pb.DiscoverRequest{
		Directories: args, // Empty = daemon uses defaults
	})
	if err != nil {
		return fmt.Errorf("discover projects: %w", err)
	}

	fmt.Printf("Scanned %d directories, found %d projects\n", resp.TotalScanned, len(resp.Discovered))

	if len(resp.Discovered) > 0 {
		for _, p := range resp.Discovered {
			tools := ""
			if len(p.DetectedTools) > 0 {
				tools = " [" + strings.Join(p.DetectedTools, ", ") + "]"
			}
			fmt.Printf("  %s  %s  %s%s\n", shortID(p.Id), p.Name, p.Path, tools)
		}
	}

	return nil
}

func runProjectTools(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, _, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	projectID, err := resolveProjectByPrefix(ctx, c, args[0])
	if err != nil {
		return err
	}

	resp, err := c.ProjectService.GetTools(ctx, &pb.GetToolsRequest{
		ProjectId: projectID,
	})
	if err != nil {
		return fmt.Errorf("get tools: %w", err)
	}

	if len(resp.Tools) == 0 {
		fmt.Println("No AI tools detected for this project.")
		return nil
	}

	fmt.Println("Detected tools:")
	for _, t := range resp.Tools {
		fmt.Printf("  - %s\n", t)
	}

	return nil
}

// resolveProjectByPrefix finds a project by ID or name prefix.
func resolveProjectByPrefix(ctx context.Context, c *client.Client, idPrefix string) (string, error) {
	resp, err := c.ProjectService.List(ctx, &pb.ListProjectsRequest{})
	if err != nil {
		return "", err
	}

	var matches []*pb.Project
	for _, p := range resp.Projects {
		// Match by ID prefix
		if p.Id == idPrefix || (len(p.Id) >= len(idPrefix) && p.Id[:len(idPrefix)] == idPrefix) {
			matches = append(matches, p)
			continue
		}
		// Match by name prefix (case-insensitive)
		if len(p.Name) >= len(idPrefix) && strings.EqualFold(p.Name[:len(idPrefix)], idPrefix) {
			matches = append(matches, p)
		}
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("project not found: %s\n\nRun 'hqssh projects' to list available projects", idPrefix)
	}
	if len(matches) > 1 {
		msg := fmt.Sprintf("ambiguous project ID '%s' matches %d projects:\n", idPrefix, len(matches))
		for _, m := range matches {
			msg += fmt.Sprintf("  %s  %s  %s\n", shortID(m.Id), m.Name, m.Path)
		}
		msg += "\nUse a longer ID prefix to be specific"
		return "", fmt.Errorf("%s", msg)
	}

	return matches[0].Id, nil
}
