package web

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// InstanceLock provides file-based locking to prevent multiple instances
type InstanceLock struct {
	lockFile *os.File
	path     string
}

// NewInstanceLock creates a new instance lock for the given config file
func NewInstanceLock(configPath string) (*InstanceLock, error) {
	// Use config directory for lock file
	configDir := filepath.Dir(configPath)
	if configDir == "." {
		configDir = ""
	}
	
	lockPath := filepath.Join(configDir, ".tailgater.lock")
	
	// Try to create/open lock file
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock file: %w", err)
	}
	
	// Try to get exclusive lock (non-blocking)
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("another instance is already running with this config")
	}
	
	return &InstanceLock{
		lockFile: file,
		path:     lockPath,
	}, nil
}

// Unlock releases the lock and removes the lock file
func (l *InstanceLock) Unlock() error {
	if l.lockFile == nil {
		return nil
	}
	
	// Release the flock
	syscall.Flock(int(l.lockFile.Fd()), syscall.LOCK_UN)
	
	// Close file
	l.lockFile.Close()
	
	// Remove lock file
	os.Remove(l.path)
	
	return nil
}
