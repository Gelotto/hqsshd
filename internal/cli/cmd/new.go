package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/gelotto/hqsshd/internal/cli/client"
	pb "github.com/gelotto/hqsshd/proto"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	newTool      string
	newProject   string
	newNoAttach  bool
)

var newCmd = &cobra.Command{
	Use:   "new",
	Short: "Create a new session",
	Long: `Create a new AI session on the remote system.

By default, creates a shell session and attaches to it immediately.
Use --tool to specify which AI tool to launch.

Examples:
  hqssh new                              # New shell session
  hqssh new --tool claude                # New Claude session
  hqssh new --tool claude --project .    # In current directory
  hqssh new --tool aider --no-attach     # Create but don't attach`,
	RunE: runNew,
}

func init() {
	newCmd.Flags().StringVarP(&newTool, "tool", "t", "shell", "Tool to launch: claude, codex, aider, shell")
	newCmd.Flags().StringVarP(&newProject, "project", "d", "", "Working directory or project path")
	newCmd.Flags().BoolVar(&newNoAttach, "no-attach", false, "Create session but don't attach")
	rootCmd.AddCommand(newCmd)
}

func runNew(cmd *cobra.Command, args []string) error {
	cfg := resolveConfig()

	if cfg.Host == "" {
		return fmt.Errorf("no host specified\n\nProvide a host using one of:\n  --host/-H flag:    hqssh new -H server.example.com\n  Environment var:   export HQSSH_HOST=server.example.com\n  Config file:       ~/.hqssh/config.yaml with default_host set")
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

	// Connect to daemon
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
	cols, rows := getTermSize()

	// Resolve project path if specified
	workingDir := newProject
	if workingDir == "." {
		// Get current directory (local) - note: this is the local current dir
		// The daemon will need to interpret "." on its end
		wd, err := os.Getwd()
		if err == nil {
			workingDir = filepath.Base(wd) // Just use the directory name as a hint
		}
	}

	// Create the session
	fmt.Fprintf(os.Stderr, "Creating %s session...\n", newTool)
	session, err := c.SessionService.Create(ctx, &pb.CreateSessionRequest{
		Tool:             newTool,
		WorkingDirectory: workingDir,
		Cols:             int32(cols),
		Rows:             int32(rows),
	})
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Session created: %s\n", shortID(session.Id))

	if newNoAttach {
		fmt.Fprintf(os.Stderr, "\nTo attach later:\n")
		fmt.Fprintf(os.Stderr, "  hqssh attach %s -H %s\n", shortID(session.Id), cfg.Host)
		return nil
	}

	// Attach to the session (reuse attach logic)
	fmt.Fprintf(os.Stderr, "Attaching to session %s...\n", shortID(session.Id))

	// Establish gRPC streams BEFORE entering raw mode
	outputStream, err := c.SessionService.Attach(ctx, &pb.AttachRequest{
		SessionId: session.Id,
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
	go handleResizeNew(ctx, c, session.Id)

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
					SessionId: session.Id,
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
	c.SessionService.Detach(detachCtx, &pb.DetachRequest{SessionId: session.Id})

	fmt.Fprintf(os.Stderr, "\nDetached from session %s\n", shortID(session.Id))
	fmt.Fprintf(os.Stderr, "Session is still running. Reattach with:\n")
	fmt.Fprintf(os.Stderr, "  hqssh attach %s -H %s\n", shortID(session.Id), cfg.Host)

	return nil
}

func getTermSize() (int, int) {
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 80, 24
	}
	return width, height
}

func handleResizeNew(ctx context.Context, c *client.Client, sessionID string) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGWINCH)
	defer signal.Stop(sigChan)

	for {
		select {
		case <-ctx.Done():
			return
		case <-sigChan:
			cols, rows := getTermSize()
			c.SessionService.Resize(ctx, &pb.ResizeRequest{
				SessionId: sessionID,
				Cols:      int32(cols),
				Rows:      int32(rows),
			})
		}
	}
}
