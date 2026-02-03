package tailer

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"tailgater/internal/config"
	"tailgater/internal/ssh"
)

// Tailer manages log tailing from multiple SSH connections
type Tailer struct {
	manager    *ssh.Manager
	config     *config.Config
	highlights []HighlightRule
	output     chan ssh.LogLine
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	mu         sync.RWMutex
	isRunning  bool
}

// HighlightRule represents a compiled highlight rule
type HighlightRule struct {
	config.HighlightRule
	Regex *regexp.Regexp
}

// Stats represents tailer statistics
type Stats struct {
	TotalLines    int64            `json:"total_lines"`
	ServerLines   map[string]int64 `json:"server_lines"`
	ServerErrors  map[string]int64 `json:"server_errors"`
	ServerWarnings map[string]int64 `json:"server_warnings"`
}

// New creates a new Tailer instance
func New(cfg *config.Config, manager *ssh.Manager) (*Tailer, error) {
	ctx, cancel := context.WithCancel(context.Background())

	t := &Tailer{
		manager: manager,
		config:  cfg,
		output:  make(chan ssh.LogLine, 1000),
		ctx:     ctx,
		cancel:  cancel,
	}

	if err := t.compileHighlights(); err != nil {
		return nil, err
	}

	return t, nil
}

func (t *Tailer) compileHighlights() error {
	rules := t.config.GetHighlights()
	t.highlights = make([]HighlightRule, 0, len(rules))

	for _, r := range rules {
		regex, err := regexp.Compile(r.Pattern)
		if err != nil {
			return fmt.Errorf("invalid pattern '%s': %w", r.Pattern, err)
		}
		t.highlights = append(t.highlights, HighlightRule{
			HighlightRule: r,
			Regex:         regex,
		})
	}

	return nil
}

// Start begins tailing logs from all connected servers
func (t *Tailer) Start() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.isRunning {
		return fmt.Errorf("tailer already running")
	}

	tailConfig := t.config.GetTailConfig()
	files := strings.Join(tailConfig.Files, " ")
	options := tailConfig.Options
	if options == "" {
		options = "-f -n 0"
	}

	cmd := fmt.Sprintf("tail %s %s", options, files)

	clients := t.manager.GetAllClients()
	for _, client := range clients {
		if err := client.Execute(cmd); err != nil {
			return fmt.Errorf("failed to start tail on %s: %w", client.Name(), err)
		}

		t.wg.Add(2)
		go t.handleOutput(client)
		go t.handleReconnect(client, cmd)
	}

	t.isRunning = true
	return nil
}

func (t *Tailer) handleOutput(client *ssh.Client) {
	defer t.wg.Done()

	localOutput := make(chan ssh.LogLine, 100)

	// Start readers
	go client.ReadOutput(t.ctx, localOutput)
	go client.ReadError(t.ctx, localOutput)

	for {
		select {
		case line := <-localOutput:
			// Apply highlighting rules
			for _, rule := range t.highlights {
				if rule.Regex.MatchString(line.Content) {
					if rule.Name == "error" || rule.Name == "fatal" {
						line.IsError = true
					} else if rule.Name == "warning" || rule.Name == "warn" {
						line.IsWarning = true
					}
					break
				}
			}

			select {
			case t.output <- line:
			case <-t.ctx.Done():
				return
			}

		case <-t.ctx.Done():
			return
		}
	}
}

func (t *Tailer) handleReconnect(client *ssh.Client, cmd string) {
	defer t.wg.Done()

	reconnectDelay := 5 * time.Second

	for {
		select {
		case <-client.ReconnectChannel():
			// Connection lost, wait and reconnect
			select {
			case <-time.After(reconnectDelay):
			case <-t.ctx.Done():
				return
			}

			// Try to reconnect
			if err := client.Disconnect(); err != nil {
				// Log error but continue
			}

			if err := client.Connect(); err != nil {
				// Send error to output
				select {
				case t.output <- ssh.LogLine{
					ServerName: client.Name(),
					Content:    fmt.Sprintf("[TAILGATER] Reconnect failed: %v", err),
					Timestamp:  time.Now(),
					IsError:    true,
				}:
				case <-t.ctx.Done():
					return
				}
				continue
			}

			// Restart tail command
			if err := client.Execute(cmd); err != nil {
				select {
				case t.output <- ssh.LogLine{
					ServerName: client.Name(),
					Content:    fmt.Sprintf("[TAILGATER] Failed to restart tail: %v", err),
					Timestamp:  time.Now(),
					IsError:    true,
				}:
				case <-t.ctx.Done():
					return
				}
				continue
			}

			// Restart output handlers
			t.wg.Add(2)
			go t.handleOutput(client)
			go t.handleReconnect(client, cmd)
			return

		case <-t.ctx.Done():
			return
		}
	}
}

// Stop stops all tailing operations
func (t *Tailer) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.isRunning {
		return
	}

	t.cancel()
	t.wg.Wait()
	close(t.output)
	t.isRunning = false
}

// Output returns the output channel
func (t *Tailer) Output() <-chan ssh.LogLine {
	return t.output
}

// IsRunning returns whether the tailer is running
func (t *Tailer) IsRunning() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.isRunning
}

// UpdateHighlights updates the highlight rules
func (t *Tailer) UpdateHighlights() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.compileHighlights()
}

// GetStats returns current statistics
func (t *Tailer) GetStats() Stats {
	// This is a simplified version - in production, you'd track actual stats
	return Stats{
		TotalLines:     0,
		ServerLines:    make(map[string]int64),
		ServerErrors:   make(map[string]int64),
		ServerWarnings: make(map[string]int64),
	}
}

// Formatter handles log line formatting with colors
type Formatter struct {
	highlights []HighlightRule
	mu         sync.RWMutex
}

// NewFormatter creates a new formatter
func NewFormatter(cfg *config.Config) (*Formatter, error) {
	rules := cfg.GetHighlights()
	highlights := make([]HighlightRule, 0, len(rules))

	for _, r := range rules {
		regex, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern '%s': %w", r.Pattern, err)
		}
		highlights = append(highlights, HighlightRule{
			HighlightRule: r,
			Regex:         regex,
		})
	}

	return &Formatter{highlights: highlights}, nil
}

// UpdateRules updates the highlight rules
func (f *Formatter) UpdateRules(cfg *config.Config) error {
	rules := cfg.GetHighlights()
	highlights := make([]HighlightRule, 0, len(rules))

	for _, r := range rules {
		regex, err := regexp.Compile(r.Pattern)
		if err != nil {
			return fmt.Errorf("invalid pattern '%s': %w", r.Pattern, err)
		}
		highlights = append(highlights, HighlightRule{
			HighlightRule: r,
			Regex:         regex,
		})
	}

	f.mu.Lock()
	f.highlights = highlights
	f.mu.Unlock()

	return nil
}

// GetHighlights returns the highlight rules
func (f *Formatter) GetHighlights() []HighlightRule {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.highlights
}
