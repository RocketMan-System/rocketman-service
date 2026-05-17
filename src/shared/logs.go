package shared

import (
	"strings"
	"sync"
	"time"
)

// LogEntry represents a single log message captured from any source.
type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Source  string    `json:"source"` // "main" or "sing-box"
	Message string    `json:"message"`
}

// RecentLogStore holds log entries with time-based retention and an item cap.
type RecentLogStore struct {
	mu        sync.Mutex
	retention time.Duration
	maxItems  int
	entries   []LogEntry
}

// NewRecentLogStore creates a new log store with the given retention and item cap.
func NewRecentLogStore(retention time.Duration, maxItems int) *RecentLogStore {
	return &RecentLogStore{
		retention: retention,
		maxItems:  maxItems,
		entries:   make([]LogEntry, 0, 256),
	}
}

// Add appends a log entry and prunes stale ones.
func (s *RecentLogStore) Add(level, source, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.entries = append(s.entries, LogEntry{
		Time:    now,
		Level:   level,
		Source:  source,
		Message: strings.TrimSpace(msg),
	})

	s.pruneNoLock(now)

	if len(s.entries) > s.maxItems {
		overflow := len(s.entries) - s.maxItems
		s.entries = append([]LogEntry(nil), s.entries[overflow:]...)
	}
}

// Last returns entries within duration, optionally filtered by source ("all" or "" = no filter).
// Entries are returned newest-first.
func (s *RecentLogStore) Last(duration time.Duration, sourceFilter string) []LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.pruneNoLock(now)

	if len(s.entries) == 0 {
		return nil
	}

	threshold := now.Add(-duration)
	var result []LogEntry
	for i := len(s.entries) - 1; i >= 0; i-- {
		e := s.entries[i]
		if e.Time.After(threshold) {
			if sourceFilter == "" || sourceFilter == "all" || e.Source == sourceFilter {
				result = append(result, e)
			}
		}
	}
	return result
}

func (s *RecentLogStore) pruneNoLock(now time.Time) {
	if len(s.entries) == 0 {
		return
	}
	threshold := now.Add(-s.retention)
	cut := 0
	for cut < len(s.entries) && s.entries[cut].Time.Before(threshold) {
		cut++
	}
	if cut > 0 {
		s.entries = append([]LogEntry(nil), s.entries[cut:]...)
	}
}

// DetectLogLevel infers a log level from message content.
func DetectLogLevel(msg string) string {
	upper := strings.ToUpper(msg)
	if strings.Contains(upper, "ERROR") || strings.Contains(upper, "FATAL") ||
		strings.Contains(upper, "PANIC") || strings.Contains(upper, "FAILED") {
		return "ERROR"
	}
	if strings.Contains(upper, "WARN") {
		return "WARN"
	}
	return "INFO"
}

// LogMirrorWriter wraps a RecentLogStore as an io.Writer for log.SetOutput.
// It tags entries with source "main".
type LogMirrorWriter struct {
	Store *RecentLogStore
}

func (w *LogMirrorWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		w.Store.Add(DetectLogLevel(msg), "main", msg)
	}
	return len(p), nil
}

// SingboxLogWriter wraps a RecentLogStore as an io.Writer for sing-box stdout/stderr.
// It tags entries with source "sing-box".
type SingboxLogWriter struct {
	Store *RecentLogStore
}

func (w *SingboxLogWriter) Write(p []byte) (n int, err error) {
	for _, line := range strings.Split(string(p), "\n") {
		msg := strings.TrimSpace(line)
		if msg != "" {
			w.Store.Add(DetectLogLevel(msg), "sing-box", msg)
		}
	}
	return len(p), nil
}
