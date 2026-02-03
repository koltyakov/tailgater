package config

import (
	"fmt"
	"os"
	"sync"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

// ServerConfig represents a single server configuration
type ServerConfig struct {
	Name       string            `yaml:"name"`
	Host       string            `yaml:"host"`
	Port       int               `yaml:"port"`
	User       string            `yaml:"user"`
	Password   string            `yaml:"password,omitempty"`
	PrivateKey string            `yaml:"private_key,omitempty"`
	KnownHosts string            `yaml:"known_hosts,omitempty"`
	Insecure   bool              `yaml:"insecure_skip_verify,omitempty"`
	Env        map[string]string `yaml:"env,omitempty"`
}

// TailConfig represents tail command configuration
type TailConfig struct {
	Files   []string `yaml:"files"`
	Options string   `yaml:"options,omitempty"`
}

// HighlightRule defines a regex pattern and its color
type HighlightRule struct {
	Name    string `yaml:"name"`
	Pattern string `yaml:"pattern"`
	Color   string `yaml:"color"`
	Bold    bool   `yaml:"bold,omitempty"`
}

// Config represents the main configuration structure
type Config struct {
	mu sync.RWMutex

	Servers    []ServerConfig  `yaml:"servers"`
	Tail       TailConfig      `yaml:"tail"`
	Highlights []HighlightRule `yaml:"highlights"`
	Web        WebConfig       `yaml:"web"`
}

// WebConfig represents web dashboard configuration
type WebConfig struct {
	Enabled bool   `yaml:"enabled"`
	Host    string `yaml:"host"`
	Port    int    `yaml:"port"`
}

// DefaultConfig returns a default configuration
func DefaultConfig() *Config {
	return &Config{
		Tail: TailConfig{
			Files:   []string{"/var/log/syslog"},
			Options: "-f -n 0",
		},
		Highlights: []HighlightRule{
			{Name: "error", Pattern: `(?i)\b(error|fatal|critical|panic)\b`, Color: "red", Bold: true},
			{Name: "warning", Pattern: `(?i)\b(warn|warning)\b`, Color: "yellow", Bold: true},
			{Name: "info", Pattern: `(?i)\b(info|information)\b`, Color: "cyan", Bold: false},
			{Name: "success", Pattern: `(?i)\b(success|ok|done|completed)\b`, Color: "green", Bold: false},
		},
		Web: WebConfig{
			Enabled: true,
			Host:    "0.0.0.0",
			Port:    8080,
		},
	}
}

// Load loads configuration from a file
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	return cfg, nil
}

// Save saves configuration to a file
func (c *Config) Save(path string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// GetServers returns a copy of servers configuration
func (c *Config) GetServers() []ServerConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()

	servers := make([]ServerConfig, len(c.Servers))
	copy(servers, c.Servers)
	return servers
}

// GetHighlights returns a copy of highlight rules
func (c *Config) GetHighlights() []HighlightRule {
	c.mu.RLock()
	defer c.mu.RUnlock()

	highlights := make([]HighlightRule, len(c.Highlights))
	copy(highlights, c.Highlights)
	return highlights
}

// GetTailConfig returns tail configuration
func (c *Config) GetTailConfig() TailConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Tail
}

// Watch starts watching the config file for changes
func (c *Config) Watch(path string, onChange func()) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create watcher: %w", err)
	}

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					newCfg, err := Load(path)
					if err == nil {
						c.mu.Lock()
						c.Servers = newCfg.Servers
						c.Tail = newCfg.Tail
						c.Highlights = newCfg.Highlights
						c.Web = newCfg.Web
						c.mu.Unlock()
						if onChange != nil {
							onChange()
						}
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				fmt.Fprintf(os.Stderr, "Config watcher error: %v\n", err)
			}
		}
	}()

	return watcher.Add(path)
}
