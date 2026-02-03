package web

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// LogStore provides SQLite-backed storage for log messages
type LogStore struct {
	db   *sql.DB
	path string
	mu   sync.RWMutex
}

// LogEntry represents a stored log entry
type LogEntry struct {
	ID         int64     `json:"id"`
	ServerName string    `json:"server_name"`
	Content    string    `json:"content"`
	Timestamp  time.Time `json:"timestamp"`
	Level      string    `json:"level"` // error, warning, info, debug
}

// NewLogStore creates a new temporary SQLite log store
func NewLogStore() (*LogStore, error) {
	// Create temp file
	tmpFile, err := os.CreateTemp("", "tailgater-logs-*.db")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp db: %w", err)
	}
	tmpFile.Close()

	return newLogStoreAtPath(tmpFile.Name(), true)
}

// NewLogStoreAtPath creates a log store at a specific path
func NewLogStoreAtPath(dbPath string) (*LogStore, error) {
	return newLogStoreAtPath(dbPath, false)
}

func newLogStoreAtPath(dbPath string, isTemp bool) (*LogStore, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		if isTemp {
			os.Remove(dbPath)
		}
		return nil, fmt.Errorf("failed to open sqlite: %w", err)
	}

	store := &LogStore{
		db:   db,
		path: dbPath,
	}

	if err := store.initSchema(); err != nil {
		db.Close()
		if isTemp {
			os.Remove(dbPath)
		}
		return nil, fmt.Errorf("failed to init schema: %w", err)
	}

	return store, nil
}

// Close closes the database and removes the temp file
func (s *LogStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db != nil {
		s.db.Close()
	}
	if s.path != "" {
		os.Remove(s.path)
	}
	return nil
}

func (s *LogStore) initSchema() error {
	schema := `
		CREATE TABLE IF NOT EXISTS logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			server_name TEXT NOT NULL,
			content TEXT NOT NULL,
			timestamp DATETIME NOT NULL,
			level TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		
		CREATE INDEX IF NOT EXISTS idx_logs_timestamp ON logs(timestamp DESC);
		CREATE INDEX IF NOT EXISTS idx_logs_level ON logs(level);
		CREATE INDEX IF NOT EXISTS idx_logs_server ON logs(server_name);
		CREATE INDEX IF NOT EXISTS idx_logs_search ON logs(content);
	`

	_, err := s.db.Exec(schema)
	return err
}

// Insert adds a new log entry to the store
func (s *LogStore) Insert(serverName, content string, timestamp time.Time, level string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec(
		"INSERT INTO logs (server_name, content, timestamp, level) VALUES (?, ?, ?, ?)",
		serverName, content, timestamp, level,
	)
	if err != nil {
		return 0, err
	}

	return result.LastInsertId()
}

// GetRecent returns the most recent logs with pagination
func (s *LogStore) GetRecent(limit, offset int) ([]LogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(
		`SELECT id, server_name, content, timestamp, level FROM logs 
		 ORDER BY timestamp DESC, id DESC LIMIT ? OFFSET ?`,
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanLogEntries(rows)
}

// GetByLevel returns logs filtered by level
func (s *LogStore) GetByLevel(level string, limit, offset int) ([]LogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(
		`SELECT id, server_name, content, timestamp, level FROM logs 
		 WHERE level = ? ORDER BY timestamp DESC, id DESC LIMIT ? OFFSET ?`,
		level, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanLogEntries(rows)
}

// Search searches logs by content (case-insensitive)
func (s *LogStore) Search(query string, limit, offset int) ([]LogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(
		`SELECT id, server_name, content, timestamp, level FROM logs 
		 WHERE content LIKE ? ORDER BY timestamp DESC, id DESC LIMIT ? OFFSET ?`,
		"%"+query+"%", limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanLogEntries(rows)
}

// SearchByLevel searches logs by content and filters by level
func (s *LogStore) SearchByLevel(query, level string, limit, offset int) ([]LogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(
		`SELECT id, server_name, content, timestamp, level FROM logs 
		 WHERE content LIKE ? AND level = ? 
		 ORDER BY timestamp DESC, id DESC LIMIT ? OFFSET ?`,
		"%"+query+"%", level, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanLogEntries(rows)
}

// GetWithFilters returns logs filtered by servers and levels
func (s *LogStore) GetWithFilters(servers, levels []string, limit, offset int) ([]LogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := "SELECT id, server_name, content, timestamp, level FROM logs WHERE 1=1"
	args := []interface{}{}

	if len(servers) > 0 {
		placeholders := make([]string, len(servers))
		for i, server := range servers {
			placeholders[i] = "?"
			args = append(args, server)
		}
		query += " AND server_name IN (" + strings.Join(placeholders, ",") + ")"
	}

	if len(levels) > 0 {
		placeholders := make([]string, len(levels))
		for i, level := range levels {
			placeholders[i] = "?"
			args = append(args, level)
		}
		query += " AND level IN (" + strings.Join(placeholders, ",") + ")"
	}

	query += " ORDER BY timestamp DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanLogEntries(rows)
}

// SearchWithFilters searches logs with server and level filters
func (s *LogStore) SearchWithFilters(searchQuery string, servers, levels []string, limit, offset int) ([]LogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := "SELECT id, server_name, content, timestamp, level FROM logs WHERE content LIKE ?"
	args := []interface{}{"%" + searchQuery + "%"}

	if len(servers) > 0 {
		placeholders := make([]string, len(servers))
		for i, server := range servers {
			placeholders[i] = "?"
			args = append(args, server)
		}
		query += " AND server_name IN (" + strings.Join(placeholders, ",") + ")"
	}

	if len(levels) > 0 {
		placeholders := make([]string, len(levels))
		for i, level := range levels {
			placeholders[i] = "?"
			args = append(args, level)
		}
		query += " AND level IN (" + strings.Join(placeholders, ",") + ")"
	}

	query += " ORDER BY timestamp DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanLogEntries(rows)
}

// GetCount returns total log count
func (s *LogStore) GetCount() (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM logs").Scan(&count)
	return count, err
}

// GetCountByLevel returns count for a specific level
func (s *LogStore) GetCountByLevel(level string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM logs WHERE level = ?", level).Scan(&count)
	return count, err
}

// PruneOld removes logs older than the specified duration
func (s *LogStore) PruneOld(olderThan time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-olderThan)
	_, err := s.db.Exec("DELETE FROM logs WHERE timestamp < ?", cutoff)
	return err
}

func scanLogEntries(rows *sql.Rows) ([]LogEntry, error) {
	var entries []LogEntry
	for rows.Next() {
		var entry LogEntry
		err := rows.Scan(&entry.ID, &entry.ServerName, &entry.Content, &entry.Timestamp, &entry.Level)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
