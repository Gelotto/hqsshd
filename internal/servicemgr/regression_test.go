package servicemgr

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A launchctl that cannot be run, or fails for a reason other than "no
// such job", must surface as an error: reporting it as "not loaded" sends
// the user to `hqssh service start`, which fails the same way.
func TestLaunchdStatusRunnerFailureIsAnError(t *testing.T) {
	svc := Service{Kind: KindLaunchd, Scope: ScopeUser, Label: "com.gelotto.hqsshd", Domain: "gui/501", UID: 501}
	cases := map[string]FakeResponse{
		"exec error":          {Err: errors.New(`exec: "launchctl": executable file not found in $PATH`)},
		"exit 127 unknown":    {Stderr: "launchctl: something unexpected", Code: 127},
		"exit 1 empty stderr": {Code: 1},
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			r := &FakeRunner{Handler: func(call string, n int) (FakeResponse, bool) {
				if strings.HasPrefix(call, "launchctl print") {
					return resp, true
				}
				return FakeResponse{}, false
			}}
			if _, err := svc.Status(context.Background(), r); err == nil {
				t.Fatalf("Status() = nil error; want an error naming launchctl")
			}
		})
	}

	// The legitimate answers still mean "not loaded", not an error.
	for _, stderr := range []string{
		`Could not find service "com.gelotto.hqsshd" in domain for user gui: 501`,
		"Could not find domain for user gui: 501",
	} {
		r := &FakeRunner{Handler: func(call string, n int) (FakeResponse, bool) {
			return FakeResponse{Stderr: stderr, Code: 113}, true
		}}
		info, err := svc.Status(context.Background(), r)
		if err != nil || info.Loaded {
			t.Errorf("stderr %q: err=%v loaded=%v; want not loaded, no error", stderr, err, info.Loaded)
		}
	}
}

func TestLogFileByScope(t *testing.T) {
	if got := LogFile(ScopeSystem); got != SystemLogFile {
		t.Errorf("system scope log = %q", got)
	}
	if got := LogFile(ScopeUser); !strings.HasSuffix(got, "/.hqssh/logs/hqsshd.log") {
		t.Errorf("user scope log = %q", got)
	}
	if got := LogFileFor(nil); got != LogFile(ScopeUser) {
		t.Errorf("LogFileFor(nil) = %q", got)
	}
}
