package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"tailgater/internal/config"
	"tailgater/internal/ssh"
	"tailgater/internal/tailer"
)

//go:embed static/* templates/*
var content embed.FS

// Server represents the web dashboard server
type Server struct {
	config    *config.Config
	tailer    *tailer.Tailer
	manager   *ssh.Manager
	upgrader  websocket.Upgrader
	clients   map[*websocket.Conn]bool
	broadcast chan LogMessage
	mu        sync.RWMutex
	server    *http.Server
	stats     ServerStats
}

// LogMessage represents a log message sent to clients
type LogMessage struct {
	ServerName string    `json:"server_name"`
	Content    string    `json:"content"`
	Timestamp  time.Time `json:"timestamp"`
	IsError    bool      `json:"is_error"`
	IsWarning  bool      `json:"is_warning"`
}

// ServerStats represents statistics for the dashboard
type ServerStats struct {
	Servers        []ServerStatus `json:"servers"`
	TotalLines     int64          `json:"total_lines"`
	TotalErrors    int64          `json:"total_errors"`
	TotalWarnings  int64          `json:"total_warnings"`
	LinesPerSecond float64        `json:"lines_per_second"`
	mu             sync.RWMutex
}

// ServerStatus represents a single server status
type ServerStatus struct {
	Name       string `json:"name"`
	Connected  bool   `json:"connected"`
	LineCount  int64  `json:"line_count"`
	ErrorCount int64  `json:"error_count"`
	WarnCount  int64  `json:"warn_count"`
}

// NewServer creates a new web server
func NewServer(cfg *config.Config, t *tailer.Tailer, manager *ssh.Manager) *Server {
	s := &Server{
		config:    cfg,
		tailer:    t,
		manager:   manager,
		clients:   make(map[*websocket.Conn]bool),
		broadcast: make(chan LogMessage, 1000),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		stats: ServerStats{
			Servers: make([]ServerStatus, 0),
		},
	}

	// Initialize server status
	for _, client := range manager.GetAllClients() {
		s.stats.Servers = append(s.stats.Servers, ServerStatus{
			Name:      client.Name(),
			Connected: client.IsConnected(),
		})
	}

	return s
}

// Start starts the web server
func (s *Server) Start() error {
	webCfg := s.config.Web

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/servers", s.handleServers)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.Handle("/static/", http.FileServer(http.FS(content)))

	s.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", webCfg.Host, webCfg.Port),
		Handler: mux,
	}

	// Start broadcaster
	go s.broadcaster()

	// Start stats updater
	go s.statsUpdater()

	// Start log consumer
	go s.consumeLogs()

	log.Printf("Web dashboard starting on http://%s:%d", webCfg.Host, webCfg.Port)
	return s.server.ListenAndServe()
}

// Stop stops the web server
func (s *Server) Stop(ctx context.Context) error {
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	tmpl, err := template.ParseFS(content, "templates/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := struct {
		Servers []config.ServerConfig
	}{
		Servers: s.config.GetServers(),
	}

	tmpl.Execute(w, data)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	s.mu.Lock()
	s.clients[conn] = true
	s.mu.Unlock()

	// Send initial stats
	s.sendStats(conn)

	// Keep connection alive and handle ping/pong
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			s.mu.Lock()
			delete(s.clients, conn)
			s.mu.Unlock()
			break
		}
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.stats.mu.RLock()
	stats := struct {
		Servers        []ServerStatus `json:"servers"`
		TotalLines     int64          `json:"total_lines"`
		TotalErrors    int64          `json:"total_errors"`
		TotalWarnings  int64          `json:"total_warnings"`
		LinesPerSecond float64        `json:"lines_per_second"`
	}{
		Servers:        s.stats.Servers,
		TotalLines:     s.stats.TotalLines,
		TotalErrors:    s.stats.TotalErrors,
		TotalWarnings:  s.stats.TotalWarnings,
		LinesPerSecond: s.stats.LinesPerSecond,
	}
	s.stats.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	servers := make([]ServerStatus, 0)

	for _, client := range s.manager.GetAllClients() {
		servers = append(servers, ServerStatus{
			Name:      client.Name(),
			Connected: client.IsConnected(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(servers)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.config.GetHighlights())
	case http.MethodPost:
		// Update config (simplified)
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) broadcaster() {
	for msg := range s.broadcast {
		s.mu.RLock()
		clients := make([]*websocket.Conn, 0, len(s.clients))
		for client := range s.clients {
			clients = append(clients, client)
		}
		s.mu.RUnlock()

		for _, client := range clients {
			if err := client.WriteJSON(msg); err != nil {
				// Remove failed client
				s.mu.Lock()
				delete(s.clients, client)
				s.mu.Unlock()
				client.Close()
			}
		}

		// Update stats
		s.updateStats(msg)
	}
}

func (s *Server) consumeLogs() {
	output := s.tailer.Output()
	for line := range output {
		msg := LogMessage{
			ServerName: line.ServerName,
			Content:    line.Content,
			Timestamp:  line.Timestamp,
			IsError:    line.IsError,
			IsWarning:  line.IsWarning,
		}

		select {
		case s.broadcast <- msg:
		default:
			// Channel full, drop message
		}
	}
}

func (s *Server) updateStats(msg LogMessage) {
	s.stats.mu.Lock()
	defer s.stats.mu.Unlock()

	s.stats.TotalLines++
	if msg.IsError {
		s.stats.TotalErrors++
	}
	if msg.IsWarning {
		s.stats.TotalWarnings++
	}

	// Update per-server stats
	found := false
	for i := range s.stats.Servers {
		if s.stats.Servers[i].Name == msg.ServerName {
			s.stats.Servers[i].LineCount++
			if msg.IsError {
				s.stats.Servers[i].ErrorCount++
			}
			if msg.IsWarning {
				s.stats.Servers[i].WarnCount++
			}
			found = true
			break
		}
	}

	if !found {
		s.stats.Servers = append(s.stats.Servers, ServerStatus{
			Name:      msg.ServerName,
			LineCount: 1,
			ErrorCount: func() int64 {
				if msg.IsError {
					return 1
				}
				return 0
			}(),
			WarnCount: func() int64 {
				if msg.IsWarning {
					return 1
				}
				return 0
			}(),
		})
	}
}

func (s *Server) statsUpdater() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Update connection status
			s.stats.mu.Lock()
			for i := range s.stats.Servers {
				if client, ok := s.manager.GetClient(s.stats.Servers[i].Name); ok {
					s.stats.Servers[i].Connected = client.IsConnected()
				}
			}
			s.stats.mu.Unlock()

			// Broadcast to all clients
			s.mu.RLock()
			clients := make([]*websocket.Conn, 0, len(s.clients))
			for client := range s.clients {
				clients = append(clients, client)
			}
			s.mu.RUnlock()

			for _, client := range clients {
				s.sendStats(client)
			}
		}
	}
}

func (s *Server) sendStats(conn *websocket.Conn) {
	s.stats.mu.RLock()
	stats := struct {
		Servers        []ServerStatus `json:"servers"`
		TotalLines     int64          `json:"total_lines"`
		TotalErrors    int64          `json:"total_errors"`
		TotalWarnings  int64          `json:"total_warnings"`
		LinesPerSecond float64        `json:"lines_per_second"`
	}{
		Servers:        s.stats.Servers,
		TotalLines:     s.stats.TotalLines,
		TotalErrors:    s.stats.TotalErrors,
		TotalWarnings:  s.stats.TotalWarnings,
		LinesPerSecond: s.stats.LinesPerSecond,
	}
	s.stats.mu.RUnlock()

	msg := struct {
		Type  string `json:"type"`
		Stats any    `json:"stats"`
	}{
		Type:  "stats",
		Stats: stats,
	}

	conn.WriteJSON(msg)
}

// Template functions
func funcMap() template.FuncMap {
	return template.FuncMap{
		"lower": strings.ToLower,
	}
}
