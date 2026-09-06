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

package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gelotto/hqsshd/internal/cli/client"
	"github.com/gelotto/hqsshd/internal/config"
	"github.com/gelotto/hqsshd/internal/pidfile"
	"github.com/gelotto/hqsshd/internal/procscan"
	"github.com/gelotto/hqsshd/internal/servicemgr"
	"github.com/gelotto/hqsshd/internal/tools"
	pb "github.com/gelotto/hqsshd/proto"
)

// daemonAnswer is what a reachable daemon told us over one transport.
type daemonAnswer struct {
	version  string
	pid      int
	uptime   time.Duration
	sessions int
	tools    []string
}

// env carries state between checks.
type env struct {
	opts Options
	ctx  context.Context

	daemonPath string // hqsshd binary found on PATH ("" if none)

	service   *servicemgr.Service
	services  []servicemgr.Service
	svcStatus *servicemgr.StatusInfo

	pidfileStatus pidfile.Status
	pidfilePid    int

	socketAnswer *daemonAnswer
	tcpAnswer    *daemonAnswer

	cfg     *config.Config
	cfgErr  error
	token   string // daemon auth_token, sent on local RPCs
	logPath string
	logTail []string
}

func (e *env) timeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(e.ctx, e.opts.Timeout)
}

func pass(name, detail string) Check { return Check{Name: name, Status: Pass, Detail: detail} }
func warn(name, detail, hint string) Check {
	return Check{Name: name, Status: Warn, Detail: detail, Hint: hint}
}
func fail(name, detail, hint string) Check {
	return Check{Name: name, Status: Fail, Detail: detail, Hint: hint}
}
func skip(name, detail string) Check { return Check{Name: name, Status: Skip, Detail: detail} }

// ---------------------------------------------------------------------------
// 1. binaries
// ---------------------------------------------------------------------------

func checkBinaries(e *env) []Check {
	const name = "binaries"
	path, err := exec.LookPath("hqsshd")
	if err != nil {
		// Maybe it sits next to hqssh
		if self, err := os.Executable(); err == nil {
			candidate := filepath.Join(filepath.Dir(self), "hqsshd")
			if info, err := os.Stat(candidate); err == nil && info.Mode()&0111 != 0 {
				path = candidate
			}
		}
	}
	if path == "" {
		return []Check{fail(name, "hqsshd binary not found on PATH",
			"install it: "+servicemgr.InstallCommand)}
	}
	e.daemonPath = path

	ctx, cancel := e.timeout()
	defer cancel()
	stdout, _, code, err := e.opts.Runner.Run(ctx, path, "--version")
	if err != nil || code != 0 {
		return []Check{fail(name, fmt.Sprintf("%s --version failed", path),
			"the binary may be corrupt or blocked; reinstall: "+servicemgr.InstallCommand)}
	}
	daemonVersion := parseVersionOutput(stdout)
	detail := fmt.Sprintf("hqssh %s, hqsshd %s (%s)", config.DaemonVersion, daemonVersion, path)
	if daemonVersion != config.DaemonVersion {
		return []Check{warn(name, detail+" — versions differ",
			"rerun the installer so both binaries match: "+servicemgr.InstallCommand)}
	}
	return []Check{pass(name, detail)}
}

// parseVersionOutput extracts the version token from "hqsshd version vX".
func parseVersionOutput(out string) string {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "unknown"
	}
	return fields[len(fields)-1]
}

// ---------------------------------------------------------------------------
// 2. path
// ---------------------------------------------------------------------------

func checkPath(e *env) []Check {
	const name = "path"
	if e.daemonPath == "" {
		return []Check{skip(name, "no hqsshd binary to locate")}
	}
	dir := filepath.Dir(e.daemonPath)
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == dir {
			return []Check{pass(name, dir+" is in PATH")}
		}
	}
	return []Check{warn(name, dir+" is not in PATH",
		fmt.Sprintf("export PATH=\"%s:$PATH\"   (add to your shell profile)", dir))}
}

// ---------------------------------------------------------------------------
// 3. service
// ---------------------------------------------------------------------------

func checkService(e *env) []Check {
	const name = "service"
	e.services = servicemgr.DetectAll()
	if len(e.services) == 0 {
		return []Check{warn(name, "no background service installed",
			"run the daemon by hand with `hqsshd`, or install the service: "+servicemgr.InstallCommand)}
	}
	svc := e.services[0]
	e.service = &svc

	var checks []Check
	if len(e.services) > 1 {
		checks = append(checks, warn(name, fmt.Sprintf("both %s and %s exist", e.services[0].UnitPath, e.services[1].UnitPath),
			"keep one: rerun the installer with the scope you want (it removes the other), or delete the unwanted plist and bootout its job"))
	}

	ctx, cancel := e.timeout()
	defer cancel()
	info, err := svc.Status(ctx, e.opts.Runner)
	if err != nil {
		return append(checks, fail(name, svc.String()+": "+err.Error(), "hqssh service status"))
	}
	e.svcStatus = &info

	switch {
	case !info.Loaded:
		hint := "hqssh service start"
		if svc.Kind == servicemgr.KindLaunchd && info.Launchd != nil && !info.Launchd.DomainFound {
			hint = servicemgr.NoGUISessionHint()
		}
		checks = append(checks, fail(name, svc.String()+" is installed but not loaded ("+info.Summary+")", hint))
	case !info.Running:
		checks = append(checks, fail(name, svc.String()+": "+info.Summary,
			"hqssh service logs -n 50   # then: hqssh service start"))
	default:
		checks = append(checks, pass(name, svc.String()+": "+info.Summary))
	}
	for _, p := range info.Problems {
		checks = append(checks, warn(name, p, "rerun the installer: "+servicemgr.InstallCommand))
	}
	if info.Launchd != nil && info.Launchd.Program != "" && e.daemonPath != "" &&
		!sameBinary(info.Launchd.Program, e.daemonPath) {
		checks = append(checks, warn(name,
			fmt.Sprintf("the service runs %s but PATH resolves hqsshd to %s", info.Launchd.Program, e.daemonPath),
			"two installs; rerun the installer (it writes the plist for its own install dir) or remove the stray copy"))
	}

	checks = append(checks, checkServicePlatform(e, svc, info)...)
	return checks
}

// sameBinary reports whether two paths name the same file (symlinks and
// relative segments resolved).
func sameBinary(a, b string) bool {
	if a == b {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return ra == rb
}

// checkServicePlatform adds launchd- and systemd-specific advice.
func checkServicePlatform(e *env, svc servicemgr.Service, info servicemgr.StatusInfo) []Check {
	var checks []Check
	switch svc.Kind {
	case servicemgr.KindLaunchd:
		if info.Launchd != nil && info.Launchd.Loaded && info.Launchd.ExitTimeout > 0 && info.Launchd.ExitTimeout < 30 {
			checks = append(checks, warn("launchd",
				fmt.Sprintf("ExitTimeOut is %ds; launchd SIGKILLs the daemon mid-shutdown if it takes longer", info.Launchd.ExitTimeout),
				"rerun the installer to get the 30s setting: "+servicemgr.InstallCommand))
		}
		if svc.Scope == servicemgr.ScopeUser {
			ctx, cancel := e.timeout()
			guiOK, _ := servicemgr.GUIDomainAvailable(ctx, e.opts.Runner, svc.UID)
			cancel()
			if !guiOK {
				checks = append(checks, warn("launchd",
					fmt.Sprintf("gui/%d is not available (no console login): LaunchAgents only start at GUI login", svc.UID),
					servicemgr.NoGUISessionHint()))
			}
			if os.Getenv("SSH_CONNECTION") != "" && guiOK {
				checks = append(checks, pass("launchd",
					"GUI session present; note a user agent still needs a console login after every reboot"))
			}
		}
	case servicemgr.KindSystemd:
		u, err := user.Current()
		if err == nil {
			ctx, cancel := e.timeout()
			linger, ok := servicemgr.LingerEnabled(ctx, e.opts.Runner, u.Username)
			cancel()
			if ok && !linger {
				checks = append(checks, warn("systemd",
					"lingering is off: the service stops at logout and does not start at boot",
					"loginctl enable-linger "+u.Username))
			}
		}
	}
	return checks
}

// ---------------------------------------------------------------------------
// 4. pidfile
// ---------------------------------------------------------------------------

func checkPidfile(e *env) []Check {
	const name = "pidfile"
	dataDir, err := servicemgr.DataDir()
	if err != nil {
		return []Check{skip(name, "cannot resolve home directory")}
	}
	path := pidfile.Path(dataDir)
	status, pid, err := pidfile.Check(path)
	if err != nil {
		return []Check{warn(name, path+": "+err.Error(), "rm "+path)}
	}
	e.pidfileStatus, e.pidfilePid = status, pid
	switch status {
	case pidfile.StatusAlive:
		return []Check{pass(name, fmt.Sprintf("%s: locked by pid %d", path, pid))}
	case pidfile.StatusStale:
		return []Check{warn(name, fmt.Sprintf("stale pidfile (pid %d wrote it; nobody holds its lock, so that daemon is gone)", pid),
			"harmless: the next start takes it over")}
	default:
		return []Check{skip(name, "no pidfile (daemon not running, or older than v1.4.0)")}
	}
}

// ---------------------------------------------------------------------------
// 5. socket / 6. tcp
// ---------------------------------------------------------------------------

func checkSocket(e *env) []Check {
	const name = "socket"
	path := e.opts.SocketPath
	err := client.ProbeSocket(path, e.opts.Timeout)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		return []Check{fail(name, "no socket at "+path, e.startHint())}
	case errors.Is(err, syscall.ECONNREFUSED):
		return []Check{fail(name, path+" exists but nothing is listening (stale socket: daemon crashed or was killed)", e.startHint())}
	default:
		return []Check{fail(name, path+": "+err.Error(), "check permissions on the socket and its directory")}
	}

	ctx, cancel := e.timeout()
	defer cancel()
	c, err := client.ConnectLocalWithToken(ctx, path, e.token)
	if err != nil {
		return []Check{fail(name, path+": "+err.Error(), "hqssh service restart")}
	}
	defer c.Close()
	ans, err := ask(ctx, c)
	if err != nil {
		return []Check{fail(name, path+" accepts connections but hqsshd does not answer: "+err.Error(),
			"hqssh service restart   # Unauthenticated means daemon.yaml's auth_token differs from the running daemon's; restart it")}
	}
	e.socketAnswer = ans
	return []Check{pass(name, fmt.Sprintf("%s: hqsshd %s %s, up %s, %d session%s",
		path, ans.version, pidLabel(ans.pid), fmtDuration(ans.uptime), ans.sessions, plural(ans.sessions)))}
}

func checkTCP(e *env) []Check {
	const name = "tcp"
	addr := e.opts.TCPAddr
	if addr == "" || e.cfgTCPDisabled() {
		return []Check{warn(name, "tcp_port is 0: the mobile app cannot connect (it uses the TCP port through the SSH tunnel)",
			"set tcp_port: 50051 in ~/.hqssh/daemon.yaml")}
	}
	conn, err := net.DialTimeout("tcp", addr, e.opts.Timeout)
	if err != nil {
		hint := "hqssh service restart   # the mobile app connects here through the SSH tunnel"
		if e.socketAnswer != nil {
			hint = fmt.Sprintf("the daemon serves the socket but the port is held by something else (it keeps retrying); find the owner: lsof -nP -iTCP:%s", portOf(addr))
		}
		return []Check{fail(name, addr+": "+errString(err), hint)}
	}
	conn.Close()

	ctx, cancel := e.timeout()
	defer cancel()
	c, err := client.ConnectTCPWithToken(ctx, addr, e.token)
	if err != nil {
		return []Check{fail(name, addr+": "+err.Error(), "hqssh service restart")}
	}
	defer c.Close()
	ans, err := ask(ctx, c)
	if err != nil {
		return []Check{fail(name, addr+" is open but hqsshd does not answer: "+err.Error(),
			"something else may own the port; run: lsof -nP -iTCP:"+portOf(addr))}
	}
	e.tcpAnswer = ans
	return []Check{pass(name, fmt.Sprintf("%s: hqsshd %s %s", addr, ans.version, pidLabel(ans.pid)))}
}

// cfgTCPDisabled peeks at the config early (checkConfig runs later).
func (e *env) cfgTCPDisabled() bool {
	cfg, err := e.loadConfig()
	return err == nil && cfg != nil && cfg.TCPPort == 0
}

func (e *env) loadConfig() (*config.Config, error) {
	if e.cfg != nil || e.cfgErr != nil {
		return e.cfg, e.cfgErr
	}
	var cfg *config.Config
	var err error
	if e.opts.ConfigPath != "" {
		cfg, err = config.LoadFromPath(e.opts.ConfigPath)
	} else {
		cfg, err = config.Load()
	}
	if err != nil {
		e.cfgErr = err
		return nil, err
	}
	e.cfg = cfg
	return cfg, nil
}

func ask(ctx context.Context, c *client.Client) (*daemonAnswer, error) {
	info, err := c.SystemService.GetInfo(ctx, &pb.Empty{})
	if err != nil {
		return nil, err
	}
	st, err := c.SystemService.GetStatus(ctx, &pb.Empty{})
	if err != nil {
		return nil, err
	}
	return &daemonAnswer{
		version:  info.GetDaemonVersion(),
		pid:      int(st.GetPid()),
		uptime:   time.Duration(st.GetUptimeSeconds()) * time.Second,
		sessions: int(st.GetActiveSessions()),
		tools:    info.GetInstalledTools(),
	}, nil
}

// ---------------------------------------------------------------------------
// 6b. boot delay (macOS user agent)
// ---------------------------------------------------------------------------

// bootDelayThreshold is how long after boot a daemon start still counts as
// "at boot".
const bootDelayThreshold = 5 * time.Minute

// checkBootDelay spots the macOS user-agent outage: after a reboot the
// agent does not run until someone logs in at the console. It is only
// measurable on the job's first spawn since bootstrap (launchd runs = 1)
// for a plist that predates this boot — a restart or a fresh install would
// otherwise look like a late start.
func checkBootDelay(e *env) []Check {
	const name = "boot"
	if e.service == nil || e.service.Kind != servicemgr.KindLaunchd || e.service.Scope != servicemgr.ScopeUser {
		return nil
	}
	if e.svcStatus == nil || e.svcStatus.Launchd == nil || e.svcStatus.Launchd.Runs != 1 {
		return nil
	}
	if e.socketAnswer == nil || e.socketAnswer.uptime <= 0 {
		return nil
	}
	bootTime, ok := procscan.BootTime()
	if !ok {
		return nil
	}
	plist, err := os.Stat(e.service.UnitPath)
	if err != nil || !plist.ModTime().Before(bootTime) {
		return nil // installed since boot: nothing to compare
	}
	started := time.Now().Add(-e.socketAnswer.uptime)
	delay := started.Sub(bootTime)
	if delay <= bootDelayThreshold {
		return []Check{pass(name, fmt.Sprintf("daemon started %s after boot", fmtDuration(delay)))}
	}
	return []Check{warn(name,
		fmt.Sprintf("after the last reboot the daemon only started %s later — a user agent waits for a GUI login, and the phone could not reach it until then", fmtDuration(delay)),
		"log in at the Mac's console after a reboot; the service starts at GUI login")}
}

// ---------------------------------------------------------------------------
// 7. consistency
// ---------------------------------------------------------------------------

func checkConsistency(e *env) []Check {
	const name = "consistency"
	if e.socketAnswer == nil || e.tcpAnswer == nil {
		return []Check{skip(name, "needs both transports answering")}
	}
	s, t := e.socketAnswer, e.tcpAnswer
	twoDaemons := false
	if s.pid > 0 && t.pid > 0 {
		twoDaemons = s.pid != t.pid
	} else {
		// Daemons older than v1.4.0 report no pid; uptime is the next best key
		diff := s.uptime - t.uptime
		if diff < 0 {
			diff = -diff
		}
		twoDaemons = diff > 5*time.Second
	}
	if twoDaemons {
		return []Check{fail(name, fmt.Sprintf("socket is served by pid %d but TCP by pid %d: two daemons are running", s.pid, t.pid),
			fmt.Sprintf("kill %d   # then: hqssh service restart", t.pid))}
	}

	var checks []Check
	if e.svcStatus != nil && e.svcStatus.Running && s.pid > 0 && e.svcStatus.PID != s.pid {
		checks = append(checks, warn(name,
			fmt.Sprintf("the service runs pid %d but pid %d answers on the socket: a daemon outside the service owns it", e.svcStatus.PID, s.pid),
			fmt.Sprintf("kill %d   # then: hqssh service restart", s.pid)))
	}
	if e.pidfileStatus == pidfile.StatusAlive && s.pid > 0 && e.pidfilePid != s.pid {
		checks = append(checks, warn(name,
			fmt.Sprintf("pidfile says %d but pid %d answers", e.pidfilePid, s.pid), "hqssh service restart"))
	}
	if len(checks) == 0 {
		if s.pid > 0 {
			checks = append(checks, pass(name, fmt.Sprintf("socket, TCP and service agree (pid %d)", s.pid)))
		} else {
			checks = append(checks, pass(name, "socket and TCP agree (matched by uptime; pre-1.4.0 daemon reports no pid)"))
		}
	}
	return checks
}

// ---------------------------------------------------------------------------
// 8. config
// ---------------------------------------------------------------------------

func checkConfig(e *env) []Check {
	const name = "config"
	path := e.opts.ConfigPath
	if path == "" {
		path, _ = config.DefaultConfigPath()
	}
	cfg, err := e.loadConfig()
	if err != nil {
		return []Check{fail(name, path+": "+err.Error(), "fix the YAML; the daemon exits with code 2 until it parses")}
	}
	if err := cfg.Validate(); err != nil {
		return []Check{fail(name, path+": "+err.Error(), "fix the value; the daemon exits with code 2 until it validates")}
	}
	var checks []Check
	if err := config.CheckFilePermissions(path); err != nil {
		checks = append(checks, warn(name, err.Error(), "chmod 600 "+path))
	}
	if max := servicemgr.MaxSocketPathLen(); len(cfg.Socket) > max {
		checks = append(checks, fail(name,
			fmt.Sprintf("socket path is %d bytes, this OS allows %d", len(cfg.Socket), max),
			"set a shorter 'socket:' in "+path))
	}
	exists := "defaults"
	if _, err := os.Stat(path); err == nil {
		exists = path
	}
	checks = append(checks, pass(name, fmt.Sprintf("%s: socket %s, tcp_port %d, log.file %s",
		exists, cfg.Socket, cfg.TCPPort, orDefault(cfg.Log.File, "stdout"))))
	return checks
}

// ---------------------------------------------------------------------------
// 9. log
// ---------------------------------------------------------------------------

func checkLog(e *env) []Check {
	const name = "log"
	// Resolution order: explicit log.file, launchd's stdout path, the
	// installer's default, journald on Linux.
	if cfg, err := e.loadConfig(); err == nil && cfg.Log.File != "" {
		e.logPath = cfg.Log.File
	} else if e.svcStatus != nil && e.svcStatus.Launchd != nil && e.svcStatus.Launchd.StdoutPath != "" {
		e.logPath = e.svcStatus.Launchd.StdoutPath
	} else if e.service != nil && e.service.Kind == servicemgr.KindSystemd {
		e.logPath = "journalctl --user -u " + e.service.Label
		ctx, cancel := e.timeout()
		defer cancel()
		out, _, _, _ := e.opts.Runner.Run(ctx, "journalctl", "--user", "-u", e.service.Label,
			"-n", fmt.Sprint(e.opts.LogLines), "--no-pager")
		e.logTail = tailLines(out, e.opts.LogLines)
		return []Check{pass(name, e.logPath+" ("+countErrors(e.logTail)+")")}
	} else {
		e.logPath = servicemgr.DefaultLogFile()
	}

	data, info, err := readTail(e.logPath, 64*1024)
	if err != nil {
		if os.IsNotExist(err) {
			return []Check{warn(name, e.logPath+" does not exist (the daemon has not logged anything yet)",
				"hqssh service start")}
		}
		return []Check{warn(name, e.logPath+": "+err.Error(), "")}
	}
	e.logTail = tailLines(string(data), e.opts.LogLines)
	return []Check{pass(name, fmt.Sprintf("%s (%s, %s)", e.logPath, countErrors(e.logTail), fmtBytes(info.Size())))}
}

// readTail returns the last maxBytes of a file (whole file when smaller),
// starting at a line boundary, without reading the rest of it.
func readTail(path string, maxBytes int64) ([]byte, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	offset := info.Size() - maxBytes
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, err
	}
	if offset > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:] // drop the partial first line
		}
	}
	return data, info, nil
}

// ---------------------------------------------------------------------------
// 10. data-dir
// ---------------------------------------------------------------------------

func checkDataDir(e *env) []Check {
	const name = "data-dir"
	dir, err := servicemgr.DataDir()
	if err != nil {
		return []Check{skip(name, "cannot resolve home directory")}
	}
	info, err := os.Stat(dir)
	if err != nil {
		return []Check{warn(name, dir+" does not exist yet (created on first start)", "")}
	}
	if !info.IsDir() {
		return []Check{fail(name, dir+" is not a directory", "mv "+dir+" "+dir+".bak")}
	}
	probe := filepath.Join(dir, ".doctor-write-test")
	if err := os.WriteFile(probe, nil, 0600); err != nil {
		return []Check{fail(name, dir+" is not writable: "+err.Error(), "chown/chmod the directory so hqsshd can write to it")}
	}
	os.Remove(probe)

	entries, _ := os.ReadDir(dir)
	var temps []string
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".tmp") {
			temps = append(temps, en.Name())
		}
	}
	if len(temps) > 0 {
		return []Check{warn(name, fmt.Sprintf("%s has leftover temp files from an interrupted write: %s", dir, strings.Join(temps, ", ")),
			"harmless: hqsshd v1.4.0+ removes them on start")}
	}
	return []Check{pass(name, dir+" is writable")}
}

// ---------------------------------------------------------------------------
// 11. lsof (darwin) / 12. codesign (darwin)
// ---------------------------------------------------------------------------

func checkLsof(e *env) []Check {
	const name = "lsof"
	if runtime.GOOS != "darwin" {
		return nil
	}
	if p := procscan.LsofPath(); p != "" {
		return []Check{pass(name, p+" (the daemon resolves it the same way)")}
	}
	return []Check{warn(name, "lsof not found: external session discovery cannot resolve working directories",
		"lsof ships with macOS at /usr/sbin/lsof; check the system installation")}
}

func checkCodesign(e *env) []Check {
	const name = "codesign"
	if runtime.GOOS != "darwin" || e.daemonPath == "" {
		return nil
	}
	if _, err := exec.LookPath("codesign"); err != nil {
		return nil
	}
	ctx, cancel := e.timeout()
	defer cancel()
	_, stderr, code, err := e.opts.Runner.Run(ctx, "codesign", "-dv", e.daemonPath)
	if err != nil || code != 0 {
		return []Check{warn(name, "hqsshd is not code-signed", "rerun the installer, which ad-hoc signs it: "+servicemgr.InstallCommand)}
	}
	ident := ""
	for _, line := range strings.Split(stderr, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Identifier="); ok {
			ident = v
		}
	}
	if ident == "" || ident == "a.out" {
		return []Check{warn(name, "hqsshd carries a linker signature with identifier "+orDefault(ident, "(none)")+"; TCC and Console show it as \"a.out\"",
			"rerun the installer, which signs it as "+servicemgr.LaunchdLabel+": "+servicemgr.InstallCommand)}
	}
	return []Check{pass(name, "signed as "+ident)}
}

// ---------------------------------------------------------------------------
// 13. tools
// ---------------------------------------------------------------------------

func checkTools(e *env) []Check {
	const name = "tools"
	cfg, err := e.loadConfig()
	if err != nil {
		return []Check{skip(name, "config unreadable")}
	}
	configured := make([]string, 0, len(cfg.Tools))
	for _, t := range cfg.Tools {
		configured = append(configured, t.Name)
	}
	if e.socketAnswer != nil {
		if len(e.socketAnswer.tools) == 0 {
			return []Check{warn(name, "the daemon detects none of the configured AI tools ("+strings.Join(configured, ", ")+")",
				"install one, or check that its directory is on the daemon's PATH (`hqssh service logs` shows the PATH the daemon added)")}
		}
		return []Check{pass(name, "daemon detects: "+strings.Join(e.socketAnswer.tools, ", "))}
	}
	// Daemon down: run the daemon's own detector (same Detect commands,
	// login-shell fallback and timeouts) from this shell.
	found := tools.NewDetector(cfg).DetectAll()
	if len(found) == 0 {
		return []Check{warn(name, "none of the configured AI tools ("+strings.Join(configured, ", ")+") detected from this shell (daemon not running; its own environment may differ)", "")}
	}
	return []Check{pass(name, "detected from this shell: "+strings.Join(found, ", ")+" (daemon not running; its own environment may differ)")}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// startHint is the advice for "no daemon", depending on the service state.
func (e *env) startHint() string {
	if e.service == nil {
		return "hqsshd   # run in the foreground, or install the service: " + servicemgr.InstallCommand
	}
	if e.svcStatus != nil && e.svcStatus.Running {
		return "the service reports a running process but it does not answer; check `hqssh service logs -n 50`, then: hqssh service restart"
	}
	return "hqssh service start"
}

func tailLines(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func countErrors(lines []string) string {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "level=ERROR") || strings.Contains(l, `"level":"ERROR"`) {
			n++
		}
	}
	if n == 0 {
		return "no errors in the last " + fmt.Sprint(len(lines)) + " lines"
	}
	return fmt.Sprintf("%d ERROR line%s in the last %d", n, plural(n), len(lines))
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	switch {
	case h >= 48:
		return fmt.Sprintf("%dd %dh", h/24, h%24)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// pidLabel renders a daemon pid, or explains its absence (daemons older
// than v1.4.0 do not report one).
func pidLabel(pid int) string {
	if pid > 0 {
		return fmt.Sprintf("pid %d", pid)
	}
	return "pid unknown (pre-1.4.0 daemon)"
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return port
}

func errString(err error) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Err.Error()
	}
	return err.Error()
}
