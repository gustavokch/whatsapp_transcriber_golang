package exclusion

import (
	"bufio"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/zap"
)

// Manager handles the exclusion list for phone numbers and group chats.
type Manager struct {
	excluded sync.Map // Stores excluded numbers/JIDs as map[string]bool
	filePath string
	logger   *zap.Logger
}

// NewManager creates a new ExclusionListManager.
func NewManager(filePath string, logger *zap.Logger) *Manager {
	m := &Manager{
		filePath: filePath,
		logger:   logger,
	}
	m.loadExcludedNumbers()
	return m
}

// loadExcludedNumbers loads numbers from the exclusion file into the map.
func (m *Manager) loadExcludedNumbers() {
	// Ensure the directory exists
	dir := filepath.Dir(m.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		m.logger.Error("Failed to create directory for exclusion file", zap.String("path", dir), zap.Error(err))
		return
	}

	file, err := os.OpenFile(m.filePath, os.O_RDONLY|os.O_CREATE, 0644)
	if err != nil {
		m.logger.Error("Failed to open exclusion file", zap.String("path", m.filePath), zap.Error(err))
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		number := scanner.Text()
		if number != "" {
			m.excluded.Store(number, true)
		}
	}

	if err := scanner.Err(); err != nil {
		m.logger.Error("Error reading exclusion file", zap.String("path", m.filePath), zap.Error(err))
	}
	m.logger.Info("Exclusion list loaded", zap.Int("count", m.Count()))
}

// saveExcludedNumbers saves the current exclusion list to the file.
func (m *Manager) saveExcludedNumbers() {
	file, err := os.Create(m.filePath)
	if err != nil {
		m.logger.Error("Failed to create exclusion file for writing", zap.String("path", m.filePath), zap.Error(err))
		return
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	m.excluded.Range(func(key, value interface{}) bool {
		_, err := writer.WriteString(key.(string) + "\n")
		if err != nil {
			m.logger.Error("Failed to write number to exclusion file", zap.String("number", key.(string)), zap.Error(err))
			return false
		}
		return true
	})
	writer.Flush()
	m.logger.Info("Exclusion list saved", zap.Int("count", m.Count()))
}

// IsExcluded checks if a number/JID is in the exclusion list.
func (m *Manager) IsExcluded(jid string) bool {
	_, ok := m.excluded.Load(jid)
	return ok
}

// Add adds a number/JID to the exclusion list.
func (m *Manager) Add(jid string) {
	if _, loaded := m.excluded.LoadOrStore(jid, true); !loaded {
		m.logger.Info("Added to exclusion list", zap.String("jid", jid))
		m.saveExcludedNumbers()
	} else {
		m.logger.Debug("JID already in exclusion list", zap.String("jid", jid))
	}
}

// Remove removes a number/JID from the exclusion list.
func (m *Manager) Remove(jid string) {
	if _, loaded := m.excluded.LoadAndDelete(jid); loaded {
		m.logger.Info("Removed from exclusion list", zap.String("jid", jid))
		m.saveExcludedNumbers()
	} else {
		m.logger.Debug("JID not found in exclusion list", zap.String("jid", jid))
	}
}

// Count returns the number of entries in the exclusion list.
func (m *Manager) Count() int {
	count := 0
	m.excluded.Range(func(key, value interface{}) bool {
		count++
		return true
	})
	return count
}

// GetAllExcluded returns all excluded numbers/JIDs as a slice of strings.
func (m *Manager) GetAllExcluded() []string {
	var excluded []string
	m.excluded.Range(func(key, value interface{}) bool {
		excluded = append(excluded, key.(string))
		return true
	})
	return excluded
}