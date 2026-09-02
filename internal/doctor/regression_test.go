package doctor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gelotto/hqsshd/internal/config"
	"github.com/gelotto/hqsshd/internal/servicemgr"
)

// The doctor must never consult the host's real service definitions when
// the caller injects its own: a developer Mac with the system daemon
// installed used to fail TestRunWithoutDaemon through this leak.
func TestRunUsesInjectedServiceDetection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	r := &servicemgr.FakeRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	calls := 0
	report := Run(ctx, Options{
		SocketPath: filepath.Join(home, "nope.sock"), TCPAddr: "127.0.0.1:1", Runner: r, Timeout: time.Second,
		DetectServices: func() []servicemgr.Service { calls++; return nil },
		DetectTools:    func(*config.Config) []string { return nil },
	})
	if calls != 1 {
		t.Errorf("DetectServices called %d times, want 1", calls)
	}
	for _, c := range r.Calls {
		if strings.HasPrefix(c, "launchctl print") || strings.HasPrefix(c, "systemctl") {
			t.Errorf("service manager queried despite no injected services: %s", c)
		}
	}
	for _, c := range report.Checks {
		if c.Name == "service" && c.Status != Warn {
			t.Errorf("service check = %+v; want WARN (no service installed)", c)
		}
	}
}

// A service manager that cannot be queried is reported as unknown (WARN),
// not as a job that is not loaded (FAIL + "hqssh service start").
func TestServiceManagerFailureIsWarn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	r := &servicemgr.FakeRunner{Handler: func(call string, n int) (servicemgr.FakeResponse, bool) {
		if strings.HasPrefix(call, "launchctl print") {
			return servicemgr.FakeResponse{Stderr: "launchctl: boom", Code: 127}, true
		}
		return servicemgr.FakeResponse{}, false
	}}
	svc := servicemgr.Service{Kind: servicemgr.KindLaunchd, Scope: servicemgr.ScopeUser, Label: "com.gelotto.hqsshd",
		Domain: "gui/501", UID: 501, UnitPath: filepath.Join(home, "com.gelotto.hqsshd.plist")}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	report := Run(ctx, Options{
		SocketPath: filepath.Join(home, "nope.sock"), TCPAddr: "127.0.0.1:1", Runner: r, Timeout: time.Second,
		DetectServices: func() []servicemgr.Service { return []servicemgr.Service{svc} },
		DetectTools:    func(*config.Config) []string { return nil },
	})
	for _, c := range report.Checks {
		if c.Name == "service" {
			if c.Status != Warn || !strings.Contains(c.Detail, "could not query") {
				t.Errorf("service check = %+v; want WARN could not query", c)
			}
			return
		}
	}
	t.Fatal("no service check in report")
}
