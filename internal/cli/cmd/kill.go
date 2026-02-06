package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gelotto/hqsshd/internal/cli/client"
	pb "github.com/gelotto/hqsshd/proto"
	"github.com/spf13/cobra"
)

var forceKill bool

var killCmd = &cobra.Command{
	Use:   "kill <session-id>",
	Short: "Terminate a session",
	Long: `Kill a running session on the remote system.

This permanently terminates the session and its underlying process.
Use with caution - any unsaved work in the session will be lost.

Examples:
  hqssh kill abc12345 -H server.example.com
  hqssh kill abc12345 --force    # Skip confirmation`,
	Args: cobra.ExactArgs(1),
	RunE: runKill,
}

func init() {
	killCmd.Flags().BoolVarP(&forceKill, "force", "f", false, "Skip confirmation prompt")
	rootCmd.AddCommand(killCmd)
}

func runKill(cmd *cobra.Command, args []string) error {
	sessionID := args[0]

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, hostLabel, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	// Resolve session ID and get session info
	fullSessionID, session, err := resolveSessionWithInfo(ctx, c, sessionID, hostLabel)
	if err != nil {
		return fmt.Errorf("find session: %w", err)
	}

	// Confirmation prompt unless --force
	if !forceKill {
		project := session.ProjectId
		if project == "" {
			project = "(shell)"
		}
		fmt.Printf("Kill session %s (%s, %s)? [y/N] ", shortID(fullSessionID), session.Tool, project)

		reader := bufio.NewReader(os.Stdin)
		response, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(response))

		if response != "y" && response != "yes" {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	// Kill the session
	_, err = c.SessionService.Kill(ctx, &pb.KillSessionRequest{
		SessionId: fullSessionID,
	})
	if err != nil {
		return fmt.Errorf("kill session: %w", err)
	}

	fmt.Printf("Session %s terminated.\n", shortID(fullSessionID))
	return nil
}

// resolveSessionWithInfo finds a session by ID prefix and returns both the full ID and session info
func resolveSessionWithInfo(ctx context.Context, c *client.Client, idPrefix, hostLabel string) (string, *pb.Session, error) {
	resp, err := c.SessionService.List(ctx, &pb.ListSessionsRequest{IncludeEnded: false})
	if err != nil {
		return "", nil, err
	}

	var matches []*pb.Session
	for _, s := range resp.Sessions {
		if s.Id == idPrefix || (len(s.Id) >= len(idPrefix) && s.Id[:len(idPrefix)] == idPrefix) {
			matches = append(matches, s)
		}
	}

	if len(matches) == 0 {
		return "", nil, fmt.Errorf("session not found: %s\n\nRun 'hqssh sessions%s' to list available sessions", idPrefix, hostFlag(hostLabel))
	}
	if len(matches) > 1 {
		var msg string
		msg = fmt.Sprintf("ambiguous session ID '%s' matches %d sessions:\n", idPrefix, len(matches))
		for _, m := range matches {
			project := m.ProjectId
			if project == "" {
				project = "(shell)"
			}
			msg += fmt.Sprintf("  %s  %s  %s\n", shortID(m.Id), m.Tool, project)
		}
		msg += "\nUse a longer ID prefix to be specific"
		return "", nil, fmt.Errorf("%s", msg)
	}

	return matches[0].Id, matches[0], nil
}
