package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/gelotto/hqsshd/internal/cli/client"
	pb "github.com/gelotto/hqsshd/proto"
)

type fakeSystemService struct {
	pb.SystemServiceClient
	info *pb.SystemInfo
	err  error
}

func (f fakeSystemService) GetInfo(ctx context.Context, in *pb.Empty, opts ...grpc.CallOption) (*pb.SystemInfo, error) {
	return f.info, f.err
}

func TestResolveTool(t *testing.T) {
	ctx := context.Background()
	daemon := func(info *pb.SystemInfo, err error) *client.Client {
		return &client.Client{SystemService: fakeSystemService{info: info, err: err}}
	}

	// No --tool: first detected AI tool.
	got, err := resolveTool(ctx, daemon(&pb.SystemInfo{InstalledTools: []string{"codex", "claude"}}, nil), "")
	if err != nil || got != "codex" {
		t.Errorf("default tool = %q, %v", got, err)
	}

	// Nothing detected: an actionable error.
	if _, err := resolveTool(ctx, daemon(&pb.SystemInfo{}, nil), ""); err == nil || !strings.Contains(err.Error(), "--tool") {
		t.Errorf("no tools: %v", err)
	}

	// A 1.5+ daemon that reports shell disabled refuses up front.
	_, err = resolveTool(ctx, daemon(&pb.SystemInfo{InstalledTools: []string{"claude"}, ShellEnabled: proto.Bool(false)}, nil), "shell")
	if err == nil || !strings.Contains(err.Error(), "enable_shell_tool") {
		t.Errorf("shell disabled: %v", err)
	}
	got, err = resolveTool(ctx, daemon(&pb.SystemInfo{ShellEnabled: proto.Bool(true)}, nil), "shell")
	if err != nil || got != "shell" {
		t.Errorf("shell enabled: %q, %v", got, err)
	}

	// A pre-1.5 daemon does not report the flag: let it decide.
	got, err = resolveTool(ctx, daemon(&pb.SystemInfo{InstalledTools: []string{"claude"}}, nil), "shell")
	if err != nil || got != "shell" {
		t.Errorf("old daemon: %q, %v", got, err)
	}

	// GetInfo failing: an explicit tool passes through, no tool is an error.
	got, err = resolveTool(ctx, daemon(nil, errors.New("boom")), "aider")
	if err != nil || got != "aider" {
		t.Errorf("info error with explicit tool: %q, %v", got, err)
	}
	if _, err := resolveTool(ctx, daemon(nil, errors.New("boom")), ""); err == nil {
		t.Error("info error without tool should fail")
	}
}
