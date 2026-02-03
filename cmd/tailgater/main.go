package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fatih/color"
	"tailgater/internal/config"
	"tailgater/internal/ssh"
	"tailgater/internal/tailer"
	"tailgater/internal/web"
)

var (
	configPath = flag.String("config", "tailgater.yaml", "Path to configuration file")
	webMode    = flag.Bool("web", false, "Run in web dashboard mode")
	cliMode    = flag.Bool("cli", true, "Run in CLI mode (default)")
	version    = flag.Bool("version", false, "Show version")
)

const (
	appVersion = "1.0.0"
	appName    = "Tailgater"
)

func main() {
	flag.Parse()

	if *version {
		fmt.Printf("%s v%s\n", appName, appVersion)
		os.Exit(0)
	}

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Expand config path
	if *configPath == "tailgater.yaml" {
		if home, err := os.UserHomeDir(); err == nil {
			globalConfig := filepath.Join(home, ".tailgater.yaml")
			if _, err := os.Stat(globalConfig); err == nil {
				*configPath = globalConfig
			}
		}
	}

	// Watch config for changes
	if err := cfg.Watch(*configPath, func() {
		log.Println("Configuration reloaded")
	}); err != nil {
		log.Printf("Warning: failed to watch config: %v", err)
	}

	// Create SSH client manager
	manager := ssh.NewManager()

	// Create SSH clients for all servers
	servers := cfg.GetServers()
	if len(servers) == 0 {
		log.Fatal("No servers configured. Please add servers to your configuration file.")
	}

	log.Printf("Configuring %d server(s)...", len(servers))

	for _, srv := range servers {
		port := srv.Port
		if port == 0 {
			port = 22
		}

		client, err := ssh.NewClient(
			srv.Name,
			srv.Host,
			port,
			srv.User,
			srv.Password,
			srv.PrivateKey,
			srv.KnownHosts,
			srv.Insecure,
		)
		if err != nil {
			log.Fatalf("Failed to create client for %s: %v", srv.Name, err)
		}
		manager.AddClient(client)
	}

	// Connect to all servers
	log.Println("Connecting to servers...")
	if err := manager.ConnectAll(); err != nil {
		log.Printf("Warning: some connections failed: %v", err)
	}

	// Create tailer
	tail, err := tailer.New(cfg, manager)
	if err != nil {
		log.Fatalf("Failed to create tailer: %v", err)
	}

	// Start tailing
	if err := tail.Start(); err != nil {
		log.Fatalf("Failed to start tailer: %v", err)
	}

	// Setup signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Determine mode
	runWeb := *webMode || cfg.Web.Enabled
	runCLI := *cliMode && !*webMode

	var wg sync.WaitGroup

	// Start CLI mode
	if runCLI {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runCLIMode(tail, cfg, ctx)
		}()
	}

	// Start Web mode
	var webServer *web.Server
	if runWeb {
		webServer = web.NewServer(cfg, tail, manager)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := webServer.Start(); err != nil {
				log.Printf("Web server error: %v", err)
			}
		}()
		
		if !runCLI {
			log.Printf("Web dashboard available at http://%s:%d", cfg.Web.Host, cfg.Web.Port)
		}
	}

	// Wait for shutdown signal
	select {
	case sig := <-sigChan:
		log.Printf("Received signal %s, shutting down...", sig)
	case <-ctx.Done():
	}

	// Cleanup
	tail.Stop()
	
	if webServer != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		webServer.Stop(shutdownCtx)
	}

	manager.DisconnectAll()
	wg.Wait()

	log.Println("Shutdown complete")
}

func runCLIMode(tail *tailer.Tailer, cfg *config.Config, ctx context.Context) {
	formatter, err := tailer.NewFormatter(cfg)
	if err != nil {
		log.Printf("Failed to create formatter: %v", err)
		return
	}

	// Pre-compile color functions
	colorFuncs := map[string]*color.Color{
		"red":     color.New(color.FgRed, color.Bold),
		"yellow":  color.New(color.FgYellow, color.Bold),
		"green":   color.New(color.FgGreen),
		"cyan":    color.New(color.FgCyan),
		"magenta": color.New(color.FgMagenta),
		"blue":    color.New(color.FgBlue),
		"white":   color.New(color.FgWhite),
	}

	serverColors := []func(...interface{}) string{
		color.New(color.FgCyan).SprintFunc(),
		color.New(color.FgGreen).SprintFunc(),
		color.New(color.FgYellow).SprintFunc(),
		color.New(color.FgMagenta).SprintFunc(),
		color.New(color.FgBlue).SprintFunc(),
		color.New(color.FgWhite).SprintFunc(),
	}

	serverColorMap := make(map[string]func(...interface{}) string)
	colorIdx := 0

	// Get unique server names for coloring
	servers := cfg.GetServers()
	for _, srv := range servers {
		if _, ok := serverColorMap[srv.Name]; !ok {
			serverColorMap[srv.Name] = serverColors[colorIdx%len(serverColors)]
			colorIdx++
		}
	}

	output := tail.Output()
	
	for {
		select {
		case line, ok := <-output:
			if !ok {
				return
			}

			// Format output: [servername] content with colors
			serverColor := serverColorMap[line.ServerName]
			if serverColor == nil {
				serverColor = color.New(color.FgWhite).SprintFunc()
			}

			content := line.Content

			// Apply highlighting
			highlights := formatter.GetHighlights()
			for _, rule := range highlights {
				if rule.Regex.MatchString(content) {
					colorFn := colorFuncs[strings.ToLower(rule.Color)]
					if colorFn == nil {
						colorFn = color.New(color.FgWhite)
					}
					if rule.Bold {
						colorFn = colorFn.Add(color.Bold)
					}

					// Highlight matching parts
					content = rule.Regex.ReplaceAllStringFunc(content, func(match string) string {
						return colorFn.Sprint(match)
					})
					break // Only apply first matching rule
				}
			}

			// Print formatted line
			fmt.Printf("%s%s\n", serverColor("["+line.ServerName+"]"), content)

		case <-ctx.Done():
			return
		}
	}
}

// highlightMatches highlights regex matches in text
func highlightMatches(text string, re *regexp.Regexp, c *color.Color) string {
	return re.ReplaceAllStringFunc(text, func(match string) string {
		return c.Sprint(match)
	})
}
