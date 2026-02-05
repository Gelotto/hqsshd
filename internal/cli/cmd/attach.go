package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gelotto/hqsshd/internal/cli/client"
	pb "github.com/gelotto/hqsshd/proto"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var attachCmd = &cobra.Command{
	Use:   "attach <session-id>",
	Short: "Attach to a session for mobile-to-desktop handoff",
	Long: `Attach to an AI session running on the remote system.

This is the core handoff feature: start a session from your mobile device,
then continue it from your desktop.

The session will remain active after you detach (Ctrl-D or close terminal).
Other clients (including your mobile device) can remain connected.`,
	Args: cobra.ExactArgs(1),
	RunE: runAttach,
}

func init() {
	// Attach-specific flags could go here
}

func runAttach(cmd *cobra.Command, args []string) error {
	sessionID := args[0]
	cfg := resolveConfig()

	if cfg.Host == "" {
		return fmt.Errorf("no host specified\n\nProvide a host using one of:\n  --host/-H flag:    hqssh attach %s -H server.example.com\n  Environment var:   export HQSSH_HOST=server.example.com\n  Config file:       ~/.hqssh/config.yaml with default_host set", sessionID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle Ctrl-C gracefully
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)
	go func() {
		<-sigChan
		fmt.Fprintf(os.Stderr, "\nDetaching...\n")
		cancel()
	}()

	// Connect to daemon with retry on transient failures
	fmt.Fprintf(os.Stderr, "Connecting to %s...\n", cfg.Host)
	c, err := client.ConnectWithRetry(ctx, client.Config{
		Host:            cfg.Host,
		Port:            cfg.Port,
		User:            cfg.User,
		KeyPath:         cfg.Key,
		Password:        cfg.Password,
		InsecureHostKey: insecureKey,
	})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer c.Close()

	// Get terminal size
	cols, rows := getTerminalSize()

	// Find session (resolve short ID)
	fullSessionID, err := resolveSessionID(ctx, c, sessionID)
	if err != nil {
		return fmt.Errorf("find session: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Attaching to session %s...\n", shortID(fullSessionID))

	// Establish gRPC streams BEFORE entering raw mode
	// This way, if they fail, the terminal is still usable

	// Attach to session (output stream)
	outputStream, err := c.SessionService.Attach(ctx, &pb.AttachRequest{
		SessionId: fullSessionID,
		Cols:      int32(cols),
		Rows:      int32(rows),
	})
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}

	// Start input stream
	inputStream, err := c.SessionService.Input(ctx)
	if err != nil {
		return fmt.Errorf("input stream: %w", err)
	}

	// NOW set terminal to raw mode (streams are established)
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Handle terminal resize
	go handleResize(ctx, c, fullSessionID)

	// WaitGroup for input goroutine cleanup
	var inputWg sync.WaitGroup

	// Read from terminal and send to session
	inputWg.Add(1)
	go func() {
		defer inputWg.Done()
		defer inputStream.CloseSend()
		buf := make([]byte, 1024)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			n, err := os.Stdin.Read(buf)
			if err != nil {
				if err != io.EOF {
					// Ignore errors during shutdown
				}
				return
			}

			if n > 0 {
				err = inputStream.Send(&pb.TerminalInput{
					SessionId: fullSessionID,
					Data:      buf[:n],
				})
				if err != nil {
					return
				}
			}
		}
	}()

	// Read from session and write to terminal
	for {
		output, err := outputStream.Recv()
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				break
			}
			// Error case: clean up goroutines before returning
			// (defer will restore terminal)
			cancel()
			inputWg.Wait()
			return fmt.Errorf("receive: %w", err)
		}

		os.Stdout.Write(output.Data)
	}

	// Cancel context to signal goroutines to stop
	cancel()

	// Wait for input goroutine to finish
	inputWg.Wait()

	// Detach
	detachCtx, detachCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer detachCancel()
	c.SessionService.Detach(detachCtx, &pb.DetachRequest{SessionId: fullSessionID})

	fmt.Fprintf(os.Stderr, "\nDetached from session %s\n", shortID(fullSessionID))
	fmt.Fprintf(os.Stderr, "Session is still running. Reattach with:\n")
	fmt.Fprintf(os.Stderr, "  hqssh attach %s -H %s\n", shortID(fullSessionID), cfg.Host)

	return nil
}

func getTerminalSize() (int, int) {
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 80, 24 // Default
	}
	return width, height
}

func resolveSessionID(ctx context.Context, c *client.Client, idPrefix string) (string, error) {
	// Try to find a session that matches the ID prefix
	resp, err := c.SessionService.List(ctx, &pb.ListSessionsRequest{IncludeEnded: false})
	if err != nil {
		return "", err
	}

	var matches []*pb.Session
	for _, s := range resp.Sessions {
		if s.Id == idPrefix || (len(s.Id) >= len(idPrefix) && s.Id[:len(idPrefix)] == idPrefix) {
			matches = append(matches, s)
		}
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("session not found: %s\n\nRun 'hqssh sessions' to list available sessions", idPrefix)
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
		return "", fmt.Errorf("%s", msg)
	}

	return matches[0].Id, nil
}

func handleResize(ctx context.Context, c *client.Client, sessionID string) {
	// Handle SIGWINCH for terminal resize
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGWINCH)
	defer signal.Stop(sigChan)

	for {
		select {
		case <-ctx.Done():
			return
		case <-sigChan:
			cols, rows := getTerminalSize()
			c.SessionService.Resize(ctx, &pb.ResizeRequest{
				SessionId: sessionID,
				Cols:      int32(cols),
				Rows:      int32(rows),
			})
		}
	}
}
