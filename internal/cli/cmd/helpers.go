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
	"golang.org/x/term"
)

// termSize returns the current terminal dimensions, falling back to 80x24.
func termSize() (cols, rows int) {
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 80, 24
	}
	return width, height
}

// resolveSession finds a session by ID prefix (active sessions only).
func resolveSession(ctx context.Context, c *client.Client, idPrefix string) (string, error) {
	return resolveSessionByPrefix(ctx, c, idPrefix, false)
}

// resolveSessionIncludeEnded finds a session by ID prefix (including ended sessions).
func resolveSessionIncludeEnded(ctx context.Context, c *client.Client, idPrefix string) (string, error) {
	return resolveSessionByPrefix(ctx, c, idPrefix, true)
}

// resolveSessionByPrefix finds a session matching the given ID prefix.
func resolveSessionByPrefix(ctx context.Context, c *client.Client, idPrefix string, includeEnded bool) (string, error) {
	resp, err := c.SessionService.List(ctx, &pb.ListSessionsRequest{IncludeEnded: includeEnded})
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
		hint := "'hqssh sessions'"
		if includeEnded {
			hint = "'hqssh sessions --all'"
		}
		return "", fmt.Errorf("session not found: %s\n\nRun %s to list available sessions", idPrefix, hint)
	}
	if len(matches) > 1 {
		msg := fmt.Sprintf("ambiguous session ID '%s' matches %d sessions:\n", idPrefix, len(matches))
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

// attachToSession attaches to an existing session, entering raw mode and
// streaming terminal I/O. Shared by both `attach` and `new` commands.
func attachToSession(ctx context.Context, cancel context.CancelFunc, c *client.Client, sessionID string, host string) error {
	cols, rows := termSize()

	// Establish gRPC streams BEFORE entering raw mode
	outputStream, err := c.SessionService.Attach(ctx, &pb.AttachRequest{
		SessionId: sessionID,
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

	// Set terminal to raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Handle terminal resize
	go handleResizeSignal(ctx, c, sessionID)

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
				return
			}

			if n > 0 {
				err = inputStream.Send(&pb.TerminalInput{
					SessionId: sessionID,
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
			cancel()
			inputWg.Wait()
			return fmt.Errorf("receive: %w", err)
		}

		os.Stdout.Write(output.Data)
	}

	cancel()
	inputWg.Wait()

	// Detach
	detachCtx, detachCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer detachCancel()
	c.SessionService.Detach(detachCtx, &pb.DetachRequest{SessionId: sessionID})

	fmt.Fprintf(os.Stderr, "\nDetached from session %s\n", shortID(sessionID))
	fmt.Fprintf(os.Stderr, "Session is still running. Reattach with:\n")
	fmt.Fprintf(os.Stderr, "  hqssh attach %s -H %s\n", shortID(sessionID), host)

	return nil
}

// handleResizeSignal handles SIGWINCH for terminal resize.
func handleResizeSignal(ctx context.Context, c *client.Client, sessionID string) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGWINCH)
	defer signal.Stop(sigChan)

	for {
		select {
		case <-ctx.Done():
			return
		case <-sigChan:
			cols, rows := termSize()
			c.SessionService.Resize(ctx, &pb.ResizeRequest{
				SessionId: sessionID,
				Cols:      int32(cols),
				Rows:      int32(rows),
			})
		}
	}
}
