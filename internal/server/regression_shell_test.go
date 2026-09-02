package server

// Review evidence test (not a product change): the daemon rejects the
// "shell" tool unless enable_shell_tool is set, while the app's task
// creator defaults to "shell" and comments "Shell is always available
// (daemon built-in)" (app/lib/features/tasks/screens/add_task_screen.dart:34,89-94).
// This test documents the daemon side of that mismatch with the default config.

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/gelotto/hqsshd/proto"
)

func TestShellToolRejectedByDefaultConfig(t *testing.T) {
	srv, cfg := newTestServer(t, t.TempDir())
	if cfg.EnableShellTool {
		t.Fatalf("expected DefaultConfig().EnableShellTool == false")
	}

	// TaskService.Create with the app's default tool selection.
	ts := &taskService{server: srv}
	_, err := ts.Create(context.Background(), &pb.CreateTaskRequest{
		Name:   "review",
		Tool:   "shell",
		Scope:  pb.TaskScope_TASK_SCOPE_SYSTEM,
		Prompt: "echo hi",
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument || !strings.Contains(st.Message(), `enable_shell_tool`) {
		t.Fatalf("Task Create(shell) = %v; want InvalidArgument naming enable_shell_tool", err)
	}
	t.Logf("Task Create(shell) with default config -> %v", err)

	// SessionService.Create with tool "shell" (the app's plain terminal
	// session path uses this tool name).
	ss := newSessionService(srv)
	_, err = ss.Create(context.Background(), &pb.CreateSessionRequest{
		Tool:             "shell",
		WorkingDirectory: "/tmp",
		Cols:             80,
		Rows:             24,
	})
	st, _ = status.FromError(err)
	if st.Code() != codes.InvalidArgument || !strings.Contains(st.Message(), `enable_shell_tool`) {
		t.Fatalf("Session Create(shell) = %v; want InvalidArgument naming enable_shell_tool", err)
	}
	t.Logf("Session Create(shell) with default config -> %v", err)

	// GetInfo tells clients whether shell is available
	if info, err := (&systemService{server: srv}).GetInfo(context.Background(), &pb.Empty{}); err != nil || info.GetShellEnabled() {
		t.Fatalf("GetInfo shell_enabled with default config = %v (err %v); want false", info.GetShellEnabled(), err)
	}

	// And with the opt-in, the same request passes validation.
	cfg.EnableShellTool = true
	if info, err := (&systemService{server: srv}).GetInfo(context.Background(), &pb.Empty{}); err != nil || !info.GetShellEnabled() {
		t.Fatalf("GetInfo shell_enabled with opt-in = %v (err %v); want true", info.GetShellEnabled(), err)
	}
	_, err = ts.Create(context.Background(), &pb.CreateTaskRequest{
		Name:   "review2",
		Tool:   "shell",
		Scope:  pb.TaskScope_TASK_SCOPE_SYSTEM,
		Prompt: "echo hi",
	})
	if err != nil {
		t.Fatalf("Task Create(shell) with enable_shell_tool=true failed: %v", err)
	}
}
