package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"tailgater/internal/config"
	"tailgater/internal/ssh"
	"tailgater/internal/tailer"

	"github.com/gorilla/websocket"
)

//go:embed static/* templates/*
var content embed.FS

// Server represents the web dashboard server
type Server struct {
	config    *config.Config
	tailer    *tailer.Tailer
	manager   *ssh.Manager
	upgrader  websocket.Upgrader
	clients   map[*websocket.Conn]*wsClient
	broadcast chan LogMessage
	mu        sync.RWMutex
	server    *http.Server
	stats     ServerStats
	logStore  *LogStore
	indexTmpl *template.Template
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

type wsClient struct {
	conn   *websocket.Conn
	writeM sync.Mutex
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

type statsPayload struct {
	Servers        []ServerStatus `json:"servers"`
	TotalLines     int64          `json:"total_lines"`
	TotalErrors    int64          `json:"total_errors"`
	TotalWarnings  int64          `json:"total_warnings"`
	LinesPerSecond float64        `json:"lines_per_second"`
}

// NewServer creates a new web server
func NewServer(cfg *config.Config, t *tailer.Tailer, manager *ssh.Manager, store *LogStore) (*Server, error) {
	tmpl, err := template.New("index.html").Funcs(funcMap()).ParseFS(content, "templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("failed to parse index template: %w", err)
	}

	s := &Server{
		config:    cfg,
		tailer:    t,
		manager:   manager,
		clients:   make(map[*websocket.Conn]*wsClient),
		broadcast: make(chan LogMessage, 1000),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		stats: ServerStats{
			Servers: make([]ServerStatus, 0),
		},
		logStore:  store,
		indexTmpl: tmpl,
	}

	// Initialize server status
	for _, client := range manager.GetAllClients() {
		s.stats.Servers = append(s.stats.Servers, ServerStatus{
			Name:      client.Name(),
			Connected: client.IsConnected(),
		})
	}

	return s, nil
}

// Start starts the web server
func (s *Server) Start() error {
	webCfg := s.config.Web
	s.ctx, s.cancel = context.WithCancel(context.Background())

	staticFS, err := fs.Sub(content, "static")
	if err != nil {
		return fmt.Errorf("failed to load static assets: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/servers", s.handleServers)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/logs/search", s.handleLogSearch)
	mux.HandleFunc("/api/logs/count", s.handleLogCount)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	s.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", webCfg.Host, webCfg.Port),
		Handler: mux,
	}

	// Start background workers.
	s.wg.Add(4)
	go s.broadcaster()
	go s.statsUpdater()
	go s.consumeLogs()
	go s.logPruner()

	log.Printf("Web dashboard starting on http://%s:%d", webCfg.Host, webCfg.Port)
	err = s.server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Stop stops the web server
func (s *Server) Stop(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	s.closeAllClients()

	var shutdownErr error
	if s.server != nil {
		shutdownErr = s.server.Shutdown(ctx)
	}

	s.wg.Wait()
	return shutdownErr
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Servers []config.ServerConfig
	}{
		Servers: s.config.GetServers(),
	}

	if err := s.indexTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	client := s.addClient(conn)
	defer s.removeClient(conn)

	// Send initial stats
	if err := s.sendStats(client); err != nil {
		return
	}

	// Keep connection alive and handle ping/pong
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		_, _, err := conn.ReadMessage()
		if err != nil {
			return
		}
	}
}

func (s *Server) addClient(conn *websocket.Conn) *wsClient {
	client := &wsClient{conn: conn}
	s.mu.Lock()
	s.clients[conn] = client
	s.mu.Unlock()
	return client
}

func (s *Server) removeClient(conn *websocket.Conn) {
	s.mu.Lock()
	client, ok := s.clients[conn]
	if ok {
		delete(s.clients, conn)
	}
	s.mu.Unlock()

	if ok {
		_ = client.conn.Close()
	}
}

func (s *Server) getClientsSnapshot() []*wsClient {
	s.mu.RLock()
	clients := make([]*wsClient, 0, len(s.clients))
	for _, client := range s.clients {
		clients = append(clients, client)
	}
	s.mu.RUnlock()
	return clients
}

func (s *Server) closeAllClients() {
	s.mu.Lock()
	clients := make([]*wsClient, 0, len(s.clients))
	for _, client := range s.clients {
		clients = append(clients, client)
	}
	s.clients = make(map[*websocket.Conn]*wsClient)
	s.mu.Unlock()

	for _, client := range clients {
		_ = client.conn.Close()
	}
}

func (s *Server) writeJSON(client *wsClient, v any) error {
	client.writeM.Lock()
	defer client.writeM.Unlock()
	return client.conn.WriteJSON(v)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := s.snapshotStats()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (s *Server) snapshotStats() statsPayload {
	s.stats.mu.RLock()
	defer s.stats.mu.RUnlock()

	servers := make([]ServerStatus, len(s.stats.Servers))
	copy(servers, s.stats.Servers)

	return statsPayload{
		Servers:        servers,
		TotalLines:     s.stats.TotalLines,
		TotalErrors:    s.stats.TotalErrors,
		TotalWarnings:  s.stats.TotalWarnings,
		LinesPerSecond: s.stats.LinesPerSecond,
	}
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

// handleLogs returns paginated logs
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Parse query parameters
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	beforeTimestamp, beforeID, err := parseLogCursor(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Parse filters
	serversParam := r.URL.Query().Get("servers")
	levelsParam := r.URL.Query().Get("levels")

	servers := parseCSVQueryParam(serversParam)
	levels := parseCSVQueryParam(levelsParam)

	var entries []LogEntry
	var queryErr error

	if beforeTimestamp != nil && beforeID != nil {
		if len(servers) > 0 || len(levels) > 0 {
			entries, queryErr = s.logStore.GetWithFiltersBefore(servers, levels, limit, *beforeTimestamp, *beforeID)
		} else {
			entries, queryErr = s.logStore.GetRecentBefore(limit, *beforeTimestamp, *beforeID)
		}
	} else {
		// Use filtered query if filters are provided
		if len(servers) > 0 || len(levels) > 0 {
			entries, queryErr = s.logStore.GetWithFilters(servers, levels, limit, offset)
		} else {
			entries, queryErr = s.logStore.GetRecent(limit, offset)
		}
	}

	if queryErr != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch logs: %v", queryErr), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

// handleLogSearch searches logs
func (s *Server) handleLogSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		http.Error(w, "Query parameter 'q' required", http.StatusBadRequest)
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	beforeTimestamp, beforeID, err := parseLogCursor(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Parse filters
	serversParam := r.URL.Query().Get("servers")
	levelsParam := r.URL.Query().Get("levels")

	servers := parseCSVQueryParam(serversParam)
	levels := parseCSVQueryParam(levelsParam)

	var entries []LogEntry
	var queryErr error

	if beforeTimestamp != nil && beforeID != nil {
		if len(servers) > 0 || len(levels) > 0 {
			entries, queryErr = s.logStore.SearchWithFiltersBefore(query, servers, levels, limit, *beforeTimestamp, *beforeID)
		} else {
			entries, queryErr = s.logStore.SearchBefore(query, limit, *beforeTimestamp, *beforeID)
		}
	} else {
		// Use filtered search if filters are provided
		if len(servers) > 0 || len(levels) > 0 {
			entries, queryErr = s.logStore.SearchWithFilters(query, servers, levels, limit, offset)
		} else {
			entries, queryErr = s.logStore.Search(query, limit, offset)
		}
	}

	if queryErr != nil {
		http.Error(w, fmt.Sprintf("Failed to search logs: %v", queryErr), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

func parseCSVQueryParam(raw string) []string {
	if raw == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		v := strings.TrimSpace(part)
		if v != "" {
			values = append(values, v)
		}
	}

	return values
}

func parseLogCursor(r *http.Request) (*time.Time, *int64, error) {
	beforeIDParam := strings.TrimSpace(r.URL.Query().Get("before_id"))
	beforeTSParam := strings.TrimSpace(r.URL.Query().Get("before_ts"))

	if beforeIDParam == "" && beforeTSParam == "" {
		return nil, nil, nil
	}

	if beforeIDParam == "" || beforeTSParam == "" {
		return nil, nil, fmt.Errorf("both before_id and before_ts are required")
	}

	beforeID, err := strconv.ParseInt(beforeIDParam, 10, 64)
	if err != nil || beforeID <= 0 {
		return nil, nil, fmt.Errorf("invalid before_id")
	}

	beforeTS, err := time.Parse(time.RFC3339Nano, beforeTSParam)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid before_ts")
	}

	return &beforeTS, &beforeID, nil
}

// handleLogCount returns total log counts
func (s *Server) handleLogCount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	level := r.URL.Query().Get("level")

	var count int64
	var err error

	if level != "" && level != "all" {
		count, err = s.logStore.GetCountByLevel(level)
	} else {
		count, err = s.logStore.GetCount()
	}

	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get count: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int64{"count": count})
}

func (s *Server) broadcaster() {
	defer s.wg.Done()

	for {
		select {
		case <-s.ctx.Done():
			return
		case msg := <-s.broadcast:
			for _, client := range s.getClientsSnapshot() {
				if err := s.writeJSON(client, msg); err != nil {
					s.removeClient(client.conn)
				}
			}

			// Update stats
			s.updateStats(msg)
		}
	}
}

func (s *Server) consumeLogs() {
	defer s.wg.Done()

	output := s.tailer.Output()
	for {
		select {
		case <-s.ctx.Done():
			return
		case line, ok := <-output:
			if !ok {
				return
			}

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
}

func (s *Server) logPruner() {
	defer s.wg.Done()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if s.logStore != nil {
				// Prune logs older than 24 hours
				if err := s.logStore.PruneOld(24 * time.Hour); err != nil {
					log.Printf("Failed to prune logs: %v", err)
				}
			}
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
	defer s.wg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			// Update connection status
			s.stats.mu.Lock()
			for i := range s.stats.Servers {
				if client, ok := s.manager.GetClient(s.stats.Servers[i].Name); ok {
					s.stats.Servers[i].Connected = client.IsConnected()
				}
			}
			s.stats.mu.Unlock()

			for _, client := range s.getClientsSnapshot() {
				if err := s.sendStats(client); err != nil {
					s.removeClient(client.conn)
				}
			}
		}
	}
}

func (s *Server) sendStats(client *wsClient) error {
	stats := s.snapshotStats()

	msg := struct {
		Type  string `json:"type"`
		Stats any    `json:"stats"`
	}{
		Type:  "stats",
		Stats: stats,
	}

	return s.writeJSON(client, msg)
}

// Template functions
func funcMap() template.FuncMap {
	return template.FuncMap{
		"lower": strings.ToLower,
	}
}
