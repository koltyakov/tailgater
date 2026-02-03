package ssh

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Client represents an SSH client connection
type Client struct {
	name       string
	config     *ssh.ClientConfig
	addr       string
	client     *ssh.Client
	session    *ssh.Session
	stdin      io.WriteCloser
	stdout     io.Reader
	stderr     io.Reader
	connected  bool
	mu         sync.RWMutex
	reconnectC chan struct{}
}

// LogLine represents a single log line with metadata
type LogLine struct {
	ServerName string
	Content    string
	Timestamp  time.Time
	IsError    bool
	IsWarning  bool
}

// NewClient creates a new SSH client
func NewClient(name, host string, port int, user, password, privateKeyPath, knownHostsPath string, insecure bool) (*Client, error) {
	config := &ssh.ClientConfig{
		User:            user,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	if !insecure && knownHostsPath != "" {
		callback, err := getHostKeyCallback(knownHostsPath)
		if err != nil {
			return nil, fmt.Errorf("failed to setup known hosts: %w", err)
		}
		config.HostKeyCallback = callback
	}

	// Try private key first, then password
	if privateKeyPath != "" {
		signer, err := loadPrivateKey(privateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load private key: %w", err)
		}
		config.Auth = append(config.Auth, ssh.PublicKeys(signer))
	}

	if password != "" {
		config.Auth = append(config.Auth, ssh.Password(password))
	}

	addr := fmt.Sprintf("%s:%d", host, port)

	return &Client{
		name:       name,
		config:     config,
		addr:       addr,
		reconnectC: make(chan struct{}, 1),
	}, nil
}

func loadPrivateKey(path string) (ssh.Signer, error) {
	// Expand home directory
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, path[2:])
	}

	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return ssh.ParsePrivateKey(key)
}

func getHostKeyCallback(knownHostsPath string) (ssh.HostKeyCallback, error) {
	if strings.HasPrefix(knownHostsPath, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		knownHostsPath = filepath.Join(home, knownHostsPath[2:])
	}

	callback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, err
	}

	return callback, nil
}

// Connect establishes the SSH connection
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return nil
	}

	client, err := ssh.Dial("tcp", c.addr, c.config)
	if err != nil {
		return fmt.Errorf("failed to dial: %w", err)
	}

	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return fmt.Errorf("failed to create session: %w", err)
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		client.Close()
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		stdin.Close()
		session.Close()
		client.Close()
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := session.StderrPipe()
	if err != nil {
		stdin.Close()
		session.Close()
		client.Close()
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	c.client = client
	c.session = session
	c.stdin = stdin
	c.stdout = stdout
	c.stderr = stderr
	c.connected = true

	return nil
}

// Disconnect closes the SSH connection
func (c *Client) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.connected = false

	if c.session != nil {
		c.session.Close()
	}
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// IsConnected returns the connection status
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// Name returns the client name
func (c *Client) Name() string {
	return c.name
}

// Execute runs a command on the remote server
func (c *Client) Execute(cmd string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected {
		return fmt.Errorf("not connected")
	}

	return c.session.Start(cmd)
}

// Wait waits for the remote command to complete
func (c *Client) Wait() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected {
		return fmt.Errorf("not connected")
	}

	return c.session.Wait()
}

// ReadOutput reads from stdout and sends lines to the output channel
func (c *Client) ReadOutput(ctx context.Context, output chan<- LogLine) {
	c.readStream(ctx, c.stdout, output, false)
}

// ReadError reads from stderr and sends lines to the output channel
func (c *Client) ReadError(ctx context.Context, output chan<- LogLine) {
	c.readStream(ctx, c.stderr, output, true)
}

func (c *Client) readStream(ctx context.Context, reader io.Reader, output chan<- LogLine, isStderr bool) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024) // 1MB max line size

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				select {
				case output <- LogLine{
					ServerName: c.name,
					Content:    fmt.Sprintf("[TAILGATER ERROR] Read error: %v", err),
					Timestamp:  time.Now(),
					IsError:    true,
				}:
				case <-ctx.Done():
				}
			}
			
			// Connection lost, trigger reconnect
			select {
			case c.reconnectC <- struct{}{}:
			default:
			}
			return
		}

		line := LogLine{
			ServerName: c.name,
			Content:    scanner.Text(),
			Timestamp:  time.Now(),
			IsError:    isStderr,
		}

		select {
		case output <- line:
		case <-ctx.Done():
			return
		}
	}
}

// ReconnectChannel returns the reconnect signal channel
func (c *Client) ReconnectChannel() <-chan struct{} {
	return c.reconnectC
}

// Manager manages multiple SSH clients
type Manager struct {
	clients map[string]*Client
	mu      sync.RWMutex
}

// NewManager creates a new SSH client manager
func NewManager() *Manager {
	return &Manager{
		clients: make(map[string]*Client),
	}
}

// AddClient adds a client to the manager
func (m *Manager) AddClient(client *Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clients[client.Name()] = client
}

// GetClient returns a client by name
func (m *Manager) GetClient(name string) (*Client, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	client, ok := m.clients[name]
	return client, ok
}

// GetAllClients returns all clients
func (m *Manager) GetAllClients() []*Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	clients := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	return clients
}

// ConnectAll connects all clients
func (m *Manager) ConnectAll() error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var errs []string
	for _, client := range m.clients {
		if err := client.Connect(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", client.Name(), err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("connection errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// DisconnectAll disconnects all clients
func (m *Manager) DisconnectAll() error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var errs []string
	for _, client := range m.clients {
		if err := client.Disconnect(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", client.Name(), err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("disconnect errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// IsHostAvailable checks if a host is reachable
func IsHostAvailable(host string, port int, timeout time.Duration) bool {
	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
