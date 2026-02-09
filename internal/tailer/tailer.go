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
	subs       map[chan ssh.LogLine]struct{}
	subsMu     sync.RWMutex
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
	TotalLines     int64            `json:"total_lines"`
	ServerLines    map[string]int64 `json:"server_lines"`
	ServerErrors   map[string]int64 `json:"server_errors"`
	ServerWarnings map[string]int64 `json:"server_warnings"`
}

// New creates a new Tailer instance
func New(cfg *config.Config, manager *ssh.Manager) (*Tailer, error) {
	ctx, cancel := context.WithCancel(context.Background())

	t := &Tailer{
		manager: manager,
		config:  cfg,
		output:  make(chan ssh.LogLine, 1000),
		subs:    make(map[chan ssh.LogLine]struct{}),
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

	t.wg.Add(1)
	go t.dispatchOutput()

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

func (t *Tailer) dispatchOutput() {
	defer t.wg.Done()

	for {
		select {
		case line, ok := <-t.output:
			if !ok {
				t.closeSubscribers()
				return
			}

			t.subsMu.RLock()
			subs := make([]chan ssh.LogLine, 0, len(t.subs))
			for ch := range t.subs {
				subs = append(subs, ch)
			}
			t.subsMu.RUnlock()

			for _, ch := range subs {
				select {
				case ch <- line:
				default:
					// Subscriber is slow; drop to protect the tailer pipeline.
				}
			}
		case <-t.ctx.Done():
			t.closeSubscribers()
			return
		}
	}
}

func (t *Tailer) closeSubscribers() {
	t.subsMu.Lock()
	defer t.subsMu.Unlock()

	for ch := range t.subs {
		close(ch)
		delete(t.subs, ch)
	}
}

// Subscribe returns a channel that receives all log lines.
func (t *Tailer) Subscribe(buffer int) <-chan ssh.LogLine {
	if buffer <= 0 {
		buffer = 256
	}

	ch := make(chan ssh.LogLine, buffer)
	t.subsMu.Lock()
	t.subs[ch] = struct{}{}
	t.subsMu.Unlock()
	return ch
}

func (t *Tailer) handleOutput(client *ssh.Client) {
	defer t.wg.Done()

	localOutput := make(chan ssh.LogLine, 100)

	var readers sync.WaitGroup
	readers.Add(2)

	go func() {
		defer readers.Done()
		client.ReadOutput(t.ctx, localOutput)
	}()
	go func() {
		defer readers.Done()
		client.ReadError(t.ctx, localOutput)
	}()
	go func() {
		readers.Wait()
		close(localOutput)
	}()

	for {
		select {
		case line, ok := <-localOutput:
			if !ok {
				return
			}

			t.applyHighlightRules(&line)

			t.publish(line)

		case <-t.ctx.Done():
			return
		}
	}
}

func (t *Tailer) applyHighlightRules(line *ssh.LogLine) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, rule := range t.highlights {
		if rule.Regex.MatchString(line.Content) {
			switch strings.ToLower(rule.Name) {
			case "error", "fatal":
				line.IsError = true
			case "warning", "warn":
				line.IsWarning = true
			}
			return
		}
	}
}

func (t *Tailer) publish(line ssh.LogLine) {
	select {
	case t.output <- line:
	case <-t.ctx.Done():
	}
}

func (t *Tailer) handleReconnect(client *ssh.Client, cmd string) {
	defer t.wg.Done()

	reconnectDelay := 5 * time.Second

	for {
		select {
		case <-client.ReconnectChannel():
			for {
				select {
				case <-t.ctx.Done():
					return
				case <-time.After(reconnectDelay):
				}

				_ = client.Disconnect()

				if err := client.Connect(); err != nil {
					t.publishReconnectError(client.Name(), fmt.Sprintf("Reconnect failed: %v", err))
					continue
				}

				if err := client.Execute(cmd); err != nil {
					t.publishReconnectError(client.Name(), fmt.Sprintf("Failed to restart tail: %v", err))
					continue
				}

				t.wg.Add(1)
				go t.handleOutput(client)
				break
			}

		case <-t.ctx.Done():
			return
		}
	}
}

func (t *Tailer) publishReconnectError(serverName, msg string) {
	select {
	case t.output <- ssh.LogLine{
		ServerName: serverName,
		Content:    "[TAILGATER] " + msg,
		Timestamp:  time.Now(),
		IsError:    true,
	}:
	default:
	}
}

// Stop stops all tailing operations
func (t *Tailer) Stop() {
	t.mu.Lock()
	if !t.isRunning {
		t.mu.Unlock()
		return
	}
	t.isRunning = false
	t.mu.Unlock()

	t.cancel()
	t.wg.Wait()
	close(t.output)
}

// Output returns the output channel
func (t *Tailer) Output() <-chan ssh.LogLine {
	return t.Subscribe(1000)
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
