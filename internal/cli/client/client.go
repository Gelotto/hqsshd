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

package client

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gelotto/hqsshd/internal/config"
	pb "github.com/gelotto/hqsshd/proto"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

const (
	daemonPort        = 50051 // default hqsshd tcp_port
	sshConnectTimeout = 30 * time.Second
	maxRetries        = 3
	initialBackoff    = 1 * time.Second
	// maxRecvMsgSize replaces grpc-go's 4 MiB default: a session's
	// scrollback replay or GetScrollback answer can be up to
	// max_scrollback_size (10 MB by default).
	maxRecvMsgSize = 16 << 20
)

// DefaultSocketPath is the default Unix socket path for the local daemon.
const DefaultSocketPath = "/tmp/hqssh.sock"

// Client connects to the daemon via SSH tunnel or local Unix socket.
type Client struct {
	sshClient  *ssh.Client
	grpcConn   *grpc.ClientConn
	listener   net.Listener
	done       chan struct{} // closed on Close() to stop keepalive goroutine
	daemonPort int           // remote hqsshd tcp_port reached through the tunnel

	SessionService pb.SessionServiceClient
	ProjectService pb.ProjectServiceClient
	SystemService  pb.SystemServiceClient
	TaskService    pb.TaskServiceClient
}

// Config holds connection configuration.
type Config struct {
	Host            string
	Port            int
	User            string
	KeyPath         string
	Password        string
	InsecureHostKey bool   // Skip host key verification (not recommended)
	DaemonPort      int    // hqsshd tcp_port on the remote host (0 = 50051)
	AuthToken       string // hqsshd auth_token on the remote host ("" = none)
}

// Connect establishes SSH connection and gRPC tunnel.
func Connect(ctx context.Context, cfg Config) (*Client, error) {
	sshConfig, err := buildSSHConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("SSH config: %w", err)
	}
	return connectWith(ctx, cfg, sshConfig)
}

// connectWith dials with an SSH config that was built once, so a retry
// never re-reads keys or prompts for a passphrase again.
func connectWith(ctx context.Context, cfg Config, sshConfig *ssh.ClientConfig) (*Client, error) {
	// Connect to SSH
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	sshClient, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		errStr := err.Error()
		if contains(errStr, "connection refused") {
			return nil, fmt.Errorf("SSH connection refused to %s\n\nCheck that:\n  - The host is correct\n  - SSH is running on port %d\n  - Firewall allows connections", addr, cfg.Port)
		}
		if contains(errStr, "no route to host") || contains(errStr, "network is unreachable") {
			return nil, fmt.Errorf("cannot reach %s\n\nCheck your network connection and that the host is correct", cfg.Host)
		}
		if contains(errStr, "permission denied") || contains(errStr, "authentication failed") {
			return nil, fmt.Errorf("SSH authentication failed to %s@%s\n\nTry:\n  -k, --key PATH    Use a specific key file\n  -p, --password    Use password authentication\n  --insecure        Skip host key verification (if that's the issue)", cfg.User, cfg.Host)
		}
		if contains(errStr, "key is unknown") {
			return nil, fmt.Errorf("host '%s' not in ~/.ssh/known_hosts\n\nRun 'ssh %s' first to add it, or use --insecure to skip verification", cfg.Host, cfg.Host)
		}
		if contains(errStr, "key mismatch") {
			return nil, fmt.Errorf("WARNING: host key for '%s' has changed!\n\nThis could indicate a man-in-the-middle attack.\nIf the server was reinstalled, update ~/.ssh/known_hosts:\n  ssh-keygen -R %s\n\nThen reconnect, or use --insecure to skip verification", cfg.Host, cfg.Host)
		}
		if contains(errStr, "key is revoked") {
			return nil, fmt.Errorf("host key for '%s' has been revoked in ~/.ssh/known_hosts", cfg.Host)
		}
		return nil, fmt.Errorf("SSH connect to %s: %w", addr, err)
	}

	// Create local listener for forwarding
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		sshClient.Close()
		return nil, fmt.Errorf("local listener: %w", err)
	}

	c := &Client{
		sshClient:  sshClient,
		listener:   listener,
		done:       make(chan struct{}),
		daemonPort: cfg.DaemonPort,
	}
	if c.daemonPort <= 0 {
		c.daemonPort = daemonPort
	}

	// Start forwarding goroutine
	go c.forwardConnections()

	// Start SSH keepalive to detect dead connections
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-t.C:
				_, _, err := sshClient.SendRequest("keepalive@openssh.com", true, nil)
				if err != nil {
					return
				}
			}
		}
	}()

	// Connect gRPC through tunnel
	grpcAddr := listener.Addr().String()
	grpcConn, err := grpc.NewClient(grpcAddr, remoteDialOptions(cfg.AuthToken)...)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("gRPC connect: %w", err)
	}
	c.grpcConn = grpcConn

	// Create service clients
	c.SessionService = pb.NewSessionServiceClient(grpcConn)
	c.ProjectService = pb.NewProjectServiceClient(grpcConn)
	c.SystemService = pb.NewSystemServiceClient(grpcConn)
	c.TaskService = pb.NewTaskServiceClient(grpcConn)

	return c, nil
}

// ConnectWithRetry attempts to connect with exponential backoff on transient failures.
// Returns after maxRetries attempts or on permanent errors (auth failure, etc).
func ConnectWithRetry(ctx context.Context, cfg Config) (*Client, error) {
	var lastErr error
	backoff := initialBackoff

	// Keys and passphrases are resolved once; a transient network failure
	// must not prompt for the passphrase again on every attempt.
	sshConfig, err := buildSSHConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("SSH config: %w", err)
	}

	for attempt := 1; attempt <= maxRetries; attempt++ {
		client, err := connectWith(ctx, cfg, sshConfig)
		if err == nil {
			return client, nil
		}

		lastErr = err

		// Check for permanent errors (don't retry)
		errStr := err.Error()
		if isPermanentError(errStr) {
			return nil, err
		}

		// Check context cancellation
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Only log retry message if we're going to retry
		if attempt < maxRetries {
			fmt.Fprintf(os.Stderr, "Connection failed (attempt %d/%d), retrying in %v...\n",
				attempt, maxRetries, backoff)

			// Wait with backoff
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}

			// Exponential backoff (1s, 2s, 4s)
			backoff *= 2
		}
	}

	return nil, fmt.Errorf("failed after %d attempts: %w", maxRetries, lastErr)
}

// isPermanentError returns true for errors that won't be fixed by retrying.
func isPermanentError(errStr string) bool {
	permanentPatterns := []string{
		"authentication failed",
		"permission denied",
		"no authentication methods",
		"host key verification failed",
		"known_hosts",
		"knownhosts",
		"key mismatch",
		"key is revoked",
	}

	for _, pattern := range permanentPatterns {
		if contains(errStr, pattern) {
			return true
		}
	}
	return false
}

// contains checks if s contains substr (case-insensitive)
func contains(s, substr string) bool {
	// Simple case-insensitive contains
	sLower := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			sLower[i] = c + 32
		} else {
			sLower[i] = c
		}
	}
	substrLower := make([]byte, len(substr))
	for i := 0; i < len(substr); i++ {
		c := substr[i]
		if c >= 'A' && c <= 'Z' {
			substrLower[i] = c + 32
		} else {
			substrLower[i] = c
		}
	}
	return bytesContains(sLower, substrLower)
}

func bytesContains(s, substr []byte) bool {
	if len(substr) == 0 {
		return true
	}
	if len(s) < len(substr) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			if s[i+j] != substr[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func (c *Client) forwardConnections() {
	for {
		localConn, err := c.listener.Accept()
		if err != nil {
			return // Listener closed
		}

		// Forward to daemon via SSH tunnel
		remoteAddr := fmt.Sprintf("127.0.0.1:%d", c.daemonPort)
		remoteConn, err := c.sshClient.Dial("tcp", remoteAddr)
		if err != nil {
			localConn.Close()
			continue
		}

		// Bidirectional copy with proper coordination
		// Cleanup goroutine handles connection closure after copy completes
		var copyWg sync.WaitGroup
		copyWg.Add(2)
		go func() {
			defer copyWg.Done()
			io.Copy(remoteConn, localConn)
		}()
		go func() {
			defer copyWg.Done()
			io.Copy(localConn, remoteConn)
		}()
		// Close connections after both copies complete (in separate goroutine to not block accept loop)
		go func(local, remote net.Conn) {
			copyWg.Wait()
			local.Close()
			remote.Close()
		}(localConn, remoteConn)
	}
}

// Close closes all connections.
// Active forwarding connections will be closed when their copy operations
// detect the closed listener/SSH client and exit.
func (c *Client) Close() error {
	if c.done != nil {
		close(c.done)
	}
	if c.grpcConn != nil {
		c.grpcConn.Close()
	}
	if c.listener != nil {
		c.listener.Close()
	}
	if c.sshClient != nil {
		c.sshClient.Close()
	}
	return nil
}

// DefaultTCPAddr is where the daemon listens for SSH-tunnelled clients.
const DefaultTCPAddr = "127.0.0.1:50051"

// ProbeSocket reports whether something is listening at the Unix socket:
// nil when a connect succeeds, an error satisfying os.IsNotExist when the
// file is absent, syscall.ECONNREFUSED (via errors.Is) when the file exists
// but no daemon owns it (crashed or SIGKILLed).
func ProbeSocket(path string, timeout time.Duration) error {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// tokenCreds sends the daemon's auth_token on every RPC, in the same
// "authorization: Bearer <token>" form the mobile app uses.
type tokenCreds struct{ token string }

func (t tokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + t.token}, nil
}

func (tokenCreds) RequireTransportSecurity() bool { return false }

// localDialOptions builds the options for a loopback connection (no TLS;
// the transport is a Unix socket or 127.0.0.1) plus the auth token when
// the daemon requires one.
func localDialOptions(token string) []grpc.DialOption {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxRecvMsgSize)),
	}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(tokenCreds{token: token}))
	}
	return opts
}

// remoteDialOptions builds the options for a connection through the SSH
// tunnel: the tunnel is the transport security, keepalives detect a dead
// link, and the remote daemon's auth token goes on every RPC.
func remoteDialOptions(token string) []grpc.DialOption {
	return append(localDialOptions(token),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                20 * time.Second,
			Timeout:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	)
}

// LocalAuthToken returns the auth_token from ~/.hqssh/daemon.yaml ("" when
// unset or unreadable). A local client runs as the daemon's user and may
// read its config, so the token never has to be typed.
func LocalAuthToken() string {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return ""
	}
	return cfg.AuthToken
}

// ConnectTCP connects to the daemon's loopback TCP listener directly (no
// SSH), sending the local auth token if the daemon requires one. Used by
// `hqssh doctor` to check the transport the mobile app uses.
func ConnectTCP(ctx context.Context, addr string) (*Client, error) {
	return ConnectTCPWithToken(ctx, addr, LocalAuthToken())
}

// ConnectTCPWithToken is ConnectTCP with an explicit auth token ("" = none).
func ConnectTCPWithToken(ctx context.Context, addr, token string) (*Client, error) {
	grpcConn, err := grpc.NewClient(addr, localDialOptions(token)...)
	if err != nil {
		return nil, fmt.Errorf("gRPC connect to %s: %w", addr, err)
	}
	return newLocalClient(grpcConn), nil
}

// ConnectLocal connects to the daemon via a Unix socket (no SSH tunnel),
// sending the local auth token if the daemon requires one.
func ConnectLocal(ctx context.Context, socketPath string) (*Client, error) {
	return ConnectLocalWithToken(ctx, socketPath, LocalAuthToken())
}

// ConnectLocalWithToken is ConnectLocal with an explicit auth token ("" = none).
func ConnectLocalWithToken(ctx context.Context, socketPath, token string) (*Client, error) {
	grpcConn, err := grpc.NewClient("unix:"+socketPath, localDialOptions(token)...)
	if err != nil {
		return nil, fmt.Errorf("gRPC connect to %s: %w", socketPath, err)
	}
	return newLocalClient(grpcConn), nil
}

func newLocalClient(grpcConn *grpc.ClientConn) *Client {

	c := &Client{
		grpcConn: grpcConn,
	}
	c.SessionService = pb.NewSessionServiceClient(grpcConn)
	c.ProjectService = pb.NewProjectServiceClient(grpcConn)
	c.SystemService = pb.NewSystemServiceClient(grpcConn)
	c.TaskService = pb.NewTaskServiceClient(grpcConn)
	return c
}

func buildSSHConfig(cfg Config) (*ssh.ClientConfig, error) {
	var authMethods []ssh.AuthMethod

	// Try key auth first. An explicit key that cannot be loaded (wrong
	// passphrase, unreadable, malformed) is an error in its own right, not
	// "no authentication methods".
	var keyProblems []string
	if cfg.KeyPath != "" {
		signer, err := loadPrivateKey(cfg.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("load key %s: %w", cfg.KeyPath, err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	} else {
		// Try default key paths; a missing file is normal, a present key
		// that fails to load is reported below.
		for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
			home, _ := os.UserHomeDir()
			keyPath := filepath.Join(home, ".ssh", name)
			signer, err := loadPrivateKey(keyPath)
			if err == nil {
				authMethods = append(authMethods, ssh.PublicKeys(signer))
				break
			}
			if !os.IsNotExist(err) {
				keyProblems = append(keyProblems, fmt.Sprintf("%s: %v", keyPath, err))
			}
		}
	}

	// Password auth
	if cfg.Password != "" {
		authMethods = append(authMethods, ssh.Password(cfg.Password))
	}

	if len(authMethods) == 0 {
		msg := "no authentication methods available\n\n" +
			"Tried default keys: ~/.ssh/id_ed25519, ~/.ssh/id_rsa, ~/.ssh/id_ecdsa\n"
		for _, p := range keyProblems {
			msg += "  could not load " + p + "\n"
		}
		msg += "\nOptions:\n" +
			"  -k, --key PATH    Specify a private key file\n" +
			"  -p, --password    Use password authentication"
		return nil, fmt.Errorf("%s", msg)
	}

	// Host key verification
	var hostKeyCallback ssh.HostKeyCallback
	if cfg.InsecureHostKey {
		hostKeyCallback = ssh.InsecureIgnoreHostKey()
		fmt.Fprintln(os.Stderr, "Warning: Host key verification disabled")
	} else if isLoopback(cfg.Host) {
		// A loopback address is normally a local port-forward whose real
		// endpoint is elsewhere; say so instead of skipping silently.
		hostKeyCallback = ssh.InsecureIgnoreHostKey()
		fmt.Fprintf(os.Stderr, "Warning: host key verification skipped for loopback host %s\n", cfg.Host)
	} else {
		// Use known_hosts file for verification
		home, _ := os.UserHomeDir()
		knownHostsPath := filepath.Join(home, ".ssh", "known_hosts")
		callback, err := knownhosts.New(knownHostsPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load known_hosts: %w\n(use --insecure to skip host key verification)", err)
		}
		hostKeyCallback = callback
	}

	return &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         sshConnectTimeout,
	}, nil
}

func loadPrivateKey(path string) (ssh.Signer, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		// Key may be passphrase-protected - check the error
		if _, ok := err.(*ssh.PassphraseMissingError); ok {
			passphrase, promptErr := promptPassphrase(path)
			if promptErr != nil {
				return nil, fmt.Errorf("passphrase prompt: %w", promptErr)
			}
			return ssh.ParsePrivateKeyWithPassphrase(key, passphrase)
		}
		return nil, err
	}
	return signer, nil
}

// isLoopback returns true if the host resolves to a loopback address.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func promptPassphrase(keyPath string) ([]byte, error) {
	fmt.Fprintf(os.Stderr, "Enter passphrase for %s: ", keyPath)
	passphrase, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr) // newline after password input
	if err != nil {
		return nil, err
	}
	return passphrase, nil
}
