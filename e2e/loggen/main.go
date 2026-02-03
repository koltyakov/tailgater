// loggen generates random log entries for e2e testing
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var (
	logFile    = flag.String("file", "/var/log/testapp.log", "Log file path")
	serverName = flag.String("server", "server", "Server name for logs")
	interval   = flag.Duration("interval", 100*time.Millisecond, "Base interval between logs")
)

var logLevels = []string{"INFO", "DEBUG", "WARN", "ERROR", "FATAL"}
var services = []string{"api", "db", "cache", "worker", "queue"}
var messages = map[string][]string{
	"INFO": {
		"Request completed successfully",
		"User login successful",
		"Cache hit for key",
		"Background job started",
		"Connection established",
		"Data synced to replica",
		"Health check passed",
	},
	"DEBUG": {
		"Processing item 12345",
		"Query executed in 15ms",
		"Cache miss, fetching from db",
		"Retry attempt 2 of 3",
		"Parsing JSON payload",
	},
	"WARN": {
		"High memory usage detected: 85%",
		"Slow query detected: 2.5s",
		"Retrying failed connection",
		"Deprecated API endpoint called",
		"Disk space low: 90% full",
		"Connection pool exhausted",
	},
	"ERROR": {
		"Failed to connect to database: connection refused",
		"HTTP 500: Internal server error",
		"Failed to parse request body: invalid JSON",
		"Authentication failed for user",
		"Critical: unable to write to disk",
		"Service timeout after 30s",
		"Memory allocation failed",
	},
	"FATAL": {
		"PANIC: runtime error: index out of range",
		"Cannot start: port already in use",
		"Failed to initialize: config missing",
		"Out of memory, shutting down",
	},
}

func main() {
	flag.Parse()

	f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open log file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	fmt.Fprintf(os.Stderr, "Log generator started: server=%s file=%s\n", *serverName, *logFile)

	for {
		select {
		case <-ticker.C:
			writeLog(f)
		case <-sigCh:
			fmt.Fprintf(os.Stderr, "Shutting down...\n")
			return
		}
	}
}

func writeLog(f *os.File) {
	level := weightedRandomLevel()
	msg := randomMessage(level)
	service := services[rand.Intn(len(services))]
	timestamp := time.Now().Format("2006-01-02 15:04:05.000")

	// Add some variety - sometimes include request ID, user ID, etc.
	var extra string
	if rand.Float32() < 0.3 {
		extra = fmt.Sprintf(" req_id=%s", randomString(8))
	}
	if rand.Float32() < 0.2 {
		extra += fmt.Sprintf(" user=%d", rand.Intn(10000))
	}
	if level == "ERROR" || level == "WARN" {
		extra += fmt.Sprintf(" retry=%d", rand.Intn(3))
	}

	logLine := fmt.Sprintf("%s [%s] %s - %s%s\n",
		timestamp, level, service, msg, extra)

	f.WriteString(logLine)
	f.Sync()
}

// weightedRandomLevel gives more weight to INFO/DEBUG, less to ERROR/FATAL
func weightedRandomLevel() string {
	r := rand.Float32()
	switch {
	case r < 0.5:
		return "INFO"
	case r < 0.75:
		return "DEBUG"
	case r < 0.9:
		return "WARN"
	case r < 0.98:
		return "ERROR"
	default:
		return "FATAL"
	}
}

func randomMessage(level string) string {
	msgs := messages[level]
	return msgs[rand.Intn(len(msgs))]
}

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}
