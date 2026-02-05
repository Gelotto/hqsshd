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

	pb "github.com/gelotto/hqsshd/proto"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	daemonPort        = 50051
	sshConnectTimeout = 30 * time.Second
	maxRetries        = 3
	initialBackoff    = 1 * time.Second
)

// Client connects to the daemon via SSH tunnel.
type Client struct {
	sshClient *ssh.Client
	grpcConn  *grpc.ClientConn
	listener  net.Listener

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
	InsecureHostKey bool // Skip host key verification (not recommended)
}

// Connect establishes SSH connection and gRPC tunnel.
func Connect(ctx context.Context, cfg Config) (*Client, error) {
	// Build SSH config
	sshConfig, err := buildSSHConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("SSH config: %w", err)
	}

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
		return nil, fmt.Errorf("SSH connect to %s: %w", addr, err)
	}

	// Create local listener for forwarding
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		sshClient.Close()
		return nil, fmt.Errorf("local listener: %w", err)
	}

	c := &Client{
		sshClient: sshClient,
		listener:  listener,
	}

	// Start forwarding goroutine
	go c.forwardConnections()

	// Connect gRPC through tunnel
	grpcAddr := listener.Addr().String()
	grpcConn, err := grpc.NewClient(grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
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

	for attempt := 1; attempt <= maxRetries; attempt++ {
		client, err := Connect(ctx, cfg)
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
		remoteAddr := fmt.Sprintf("127.0.0.1:%d", daemonPort)
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

func buildSSHConfig(cfg Config) (*ssh.ClientConfig, error) {
	var authMethods []ssh.AuthMethod

	// Try key auth first
	if cfg.KeyPath != "" {
		signer, err := loadPrivateKey(cfg.KeyPath)
		if err == nil {
			authMethods = append(authMethods, ssh.PublicKeys(signer))
		}
	} else {
		// Try default key paths
		for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
			home, _ := os.UserHomeDir()
			keyPath := filepath.Join(home, ".ssh", name)
			signer, err := loadPrivateKey(keyPath)
			if err == nil {
				authMethods = append(authMethods, ssh.PublicKeys(signer))
				break
			}
		}
	}

	// Password auth
	if cfg.Password != "" {
		authMethods = append(authMethods, ssh.Password(cfg.Password))
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication methods available\n\n" +
			"Tried default keys: ~/.ssh/id_ed25519, ~/.ssh/id_rsa, ~/.ssh/id_ecdsa\n\n" +
			"Options:\n" +
			"  -k, --key PATH    Specify a private key file\n" +
			"  -p, --password    Use password authentication")
	}

	// Host key verification
	var hostKeyCallback ssh.HostKeyCallback
	if cfg.InsecureHostKey {
		hostKeyCallback = ssh.InsecureIgnoreHostKey()
		fmt.Fprintln(os.Stderr, "Warning: Host key verification disabled")
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

func promptPassphrase(keyPath string) ([]byte, error) {
	fmt.Fprintf(os.Stderr, "Enter passphrase for %s: ", keyPath)
	passphrase, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr) // newline after password input
	if err != nil {
		return nil, err
	}
	return passphrase, nil
}
