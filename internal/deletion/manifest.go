// Package deletion provides safe, staged email deletion from Gmail.
package deletion

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wesm/msgvault/internal/fileutil"
)

// Status represents the state of a deletion batch.
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	StatusCancelled  Status = "cancelled"
)

// Method represents how messages are deleted.
type Method string

const (
	MethodTrash  Method = "trash"  // Move to Gmail trash (30-day recovery)
	MethodDelete Method = "delete" // Permanent deletion
)

// Filters specifies criteria for selecting messages.
type Filters struct {
	Senders       []string `json:"senders,omitempty"`
	SenderDomains []string `json:"sender_domains,omitempty"`
	Recipients    []string `json:"recipients,omitempty"`
	Labels        []string `json:"labels,omitempty"`
	After         string   `json:"after,omitempty"`  // ISO date
	Before        string   `json:"before,omitempty"` // ISO date
	Account       string   `json:"account,omitempty"`
}

// Summary contains statistics about messages to be deleted.
type Summary struct {
	MessageCount   int           `json:"message_count"`
	TotalSizeBytes int64         `json:"total_size_bytes"`
	DateRange      [2]string     `json:"date_range"` // [earliest, latest]
	Accounts       []string      `json:"accounts"`
	TopSenders     []SenderCount `json:"top_senders"`
}

// SenderCount represents a sender and their message count.
type SenderCount struct {
	Sender string `json:"sender"`
	Count  int    `json:"count"`
}

// Execution tracks progress of a deletion operation.
type Execution struct {
	StartedAt          time.Time  `json:"started_at"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
	Method             Method     `json:"method"`
	Succeeded          int        `json:"succeeded"`
	Failed             int        `json:"failed"`
	FailedIDs          []string   `json:"failed_ids,omitempty"`
	LastProcessedIndex int        `json:"last_processed_index"` // For resumability
}

// Manifest represents a deletion batch.
type Manifest struct {
	Version     int        `json:"version"`
	ID          string     `json:"id"`
	CreatedAt   time.Time  `json:"created_at"`
	CreatedBy   string     `json:"created_by"` // "tui", "cli", "api"
	Description string     `json:"description"`
	Filters     Filters    `json:"filters"`
	Summary     *Summary   `json:"summary,omitempty"`
	GmailIDs    []string   `json:"gmail_ids"`
	Status      Status     `json:"status"`
	Execution   *Execution `json:"execution,omitempty"`
}

// NewManifest creates a new deletion manifest.
func NewManifest(description string, gmailIDs []string) *Manifest {
	return &Manifest{
		Version:     1,
		ID:          generateID(description),
		CreatedAt:   time.Now(),
		CreatedBy:   "cli",
		Description: description,
		GmailIDs:    gmailIDs,
		Status:      StatusPending,
	}
}

// generateID creates a manifest ID from timestamp and description.
func generateID(description string) string {
	ts := time.Now().Format("20060102-150405")
	// Sanitize description for filename
	sanitized := sanitizeForFilename(description)
	if sanitized == "" {
		sanitized = "batch"
	}
	if len(sanitized) > 20 {
		sanitized = sanitized[:20]
	}
	return fmt.Sprintf("%s-%s", ts, sanitized)
}

// sanitizeForFilename removes characters unsafe for filenames.
func sanitizeForFilename(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_':
			return r
		case r == ' ' || r == '.':
			return '-'
		default:
			return -1
		}
	}, s)
}

// LoadManifest reads a manifest from a JSON file.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}

	return &m, nil
}

// Save writes the manifest to a JSON file.
func (m *Manifest) Save(path string) error {
	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}

	return fileutil.SecureWriteFile(path, data, 0600)
}

// FormatSummary returns a human-readable summary of the deletion.
func (m *Manifest) FormatSummary() string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "Deletion Batch: %s\n", m.ID)
	fmt.Fprintf(&sb, "Status: %s\n", m.Status)
	fmt.Fprintf(&sb, "Created: %s\n", m.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(&sb, "Description: %s\n", m.Description)
	fmt.Fprintf(&sb, "Messages: %d\n", len(m.GmailIDs))

	if m.Summary != nil {
		fmt.Fprintf(&sb, "Total Size: %.2f MB\n", float64(m.Summary.TotalSizeBytes)/(1024*1024))
		if len(m.Summary.DateRange) == 2 && m.Summary.DateRange[0] != "" {
			fmt.Fprintf(&sb, "Date Range: %s to %s\n", m.Summary.DateRange[0], m.Summary.DateRange[1])
		}
		if len(m.Summary.TopSenders) > 0 {
			fmt.Fprintf(&sb, "\nTop Senders:\n")
			for i, s := range m.Summary.TopSenders {
				if i >= 10 {
					break
				}
				fmt.Fprintf(&sb, "  %s: %d messages\n", s.Sender, s.Count)
			}
		}
	}

	if m.Execution != nil {
		fmt.Fprintf(&sb, "\nExecution:\n")
		fmt.Fprintf(&sb, "  Method: %s\n", m.Execution.Method)
		fmt.Fprintf(&sb, "  Succeeded: %d\n", m.Execution.Succeeded)
		fmt.Fprintf(&sb, "  Failed: %d\n", m.Execution.Failed)
		if m.Execution.CompletedAt != nil {
			fmt.Fprintf(&sb, "  Completed: %s\n", m.Execution.CompletedAt.Format(time.RFC3339))
		}
	}

	return sb.String()
}

// statusDirMap provides an explicit mapping from Status to on-disk directory name.
// This decouples the Status constant values (which may be used for display or JSON)
// from the filesystem directory names.
var statusDirMap = map[Status]string{
	StatusPending:    "pending",
	StatusInProgress: "in_progress",
	StatusCompleted:  "completed",
	StatusFailed:     "failed",
	StatusCancelled:  "cancelled",
}

// persistedStatuses lists all statuses that have on-disk directories.
var persistedStatuses = []Status{
	StatusPending, StatusInProgress, StatusCompleted, StatusFailed, StatusCancelled,
}

// Manager handles deletion manifest files.
type Manager struct {
	baseDir string // ~/.msgvault/deletions
}

// NewManager creates a deletion manager.
func NewManager(baseDir string) (*Manager, error) {
	m := &Manager{baseDir: baseDir}

	for _, status := range persistedStatuses {
		if err := os.MkdirAll(m.dirForStatus(status), 0755); err != nil {
			return nil, fmt.Errorf("create dir for %s: %w", status, err)
		}
	}

	return m, nil
}

// dirForStatus returns the directory path for a given status.
// Uses explicit mapping to decouple Status values from directory names.
func (m *Manager) dirForStatus(s Status) string {
	dirName, ok := statusDirMap[s]
	if !ok {
		// Fallback for unknown status; log warning and use status string.
		// This should not happen in normal operation.
		log.Printf("WARNING: unknown status %q has no directory mapping, using status value as directory name", s)
		dirName = string(s)
	}
	return filepath.Join(m.baseDir, dirName)
}

// PendingDir returns the path to the pending directory.
func (m *Manager) PendingDir() string { return m.dirForStatus(StatusPending) }

// InProgressDir returns the path to the in_progress directory.
func (m *Manager) InProgressDir() string { return m.dirForStatus(StatusInProgress) }

// CompletedDir returns the path to the completed directory.
func (m *Manager) CompletedDir() string { return m.dirForStatus(StatusCompleted) }

// FailedDir returns the path to the failed directory.
func (m *Manager) FailedDir() string { return m.dirForStatus(StatusFailed) }

// ListPending returns all pending deletion manifests.
func (m *Manager) ListPending() ([]*Manifest, error) {
	return m.listManifests(m.dirForStatus(StatusPending))
}

// ListInProgress returns all in-progress deletion manifests.
func (m *Manager) ListInProgress() ([]*Manifest, error) {
	return m.listManifests(m.dirForStatus(StatusInProgress))
}

// ListCompleted returns all completed deletion manifests.
func (m *Manager) ListCompleted() ([]*Manifest, error) {
	return m.listManifests(m.dirForStatus(StatusCompleted))
}

// ListFailed returns all failed deletion manifests.
func (m *Manager) ListFailed() ([]*Manifest, error) {
	return m.listManifests(m.dirForStatus(StatusFailed))
}

// ListCancelled returns all cancelled deletion manifests.
func (m *Manager) ListCancelled() ([]*Manifest, error) {
	return m.listManifests(m.dirForStatus(StatusCancelled))
}

func (m *Manager) listManifests(dir string) ([]*Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var manifests []*Manifest
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}

		path := filepath.Join(dir, e.Name())
		manifest, err := LoadManifest(path)
		if err != nil {
			log.Printf("WARNING: skipping invalid manifest %s: %v", path, err)
			continue
		}
		manifests = append(manifests, manifest)
	}

	// Sort by created time, newest first
	sort.Slice(manifests, func(i, j int) bool {
		return manifests[i].CreatedAt.After(manifests[j].CreatedAt)
	})

	return manifests, nil
}

// GetManifest loads a manifest by ID from any status directory.
func (m *Manager) GetManifest(id string) (*Manifest, string, error) {
	filename := id + ".json"
	for _, status := range persistedStatuses {
		dir := m.dirForStatus(status)
		path := filepath.Join(dir, filename)
		if manifest, err := LoadManifest(path); err == nil {
			return manifest, path, nil
		}
	}

	return nil, "", fmt.Errorf("manifest %s not found", id)
}

// SaveManifest saves a manifest to the appropriate directory based on status.
func (m *Manager) SaveManifest(manifest *Manifest) error {
	status := manifest.Status
	if !isPersistedStatus(status) {
		status = StatusPending
	}
	dir := m.dirForStatus(status)
	path := filepath.Join(dir, manifest.ID+".json")
	return manifest.Save(path)
}

// isPersistedStatus returns true if the status has a known on-disk directory.
func isPersistedStatus(s Status) bool {
	for _, ps := range persistedStatuses {
		if s == ps {
			return true
		}
	}
	return false
}

// MoveManifest moves a manifest from one status directory to another.
func (m *Manager) MoveManifest(id string, fromStatus, toStatus Status) error {
	switch fromStatus {
	case StatusPending, StatusInProgress:
		// allowed
	default:
		return fmt.Errorf("cannot move from status %s", fromStatus)
	}

	switch toStatus {
	case StatusInProgress, StatusCompleted, StatusFailed, StatusCancelled:
		// allowed
	default:
		return fmt.Errorf("cannot move to status %s", toStatus)
	}

	fromPath := filepath.Join(m.dirForStatus(fromStatus), id+".json")
	toPath := filepath.Join(m.dirForStatus(toStatus), id+".json")
	return os.Rename(fromPath, toPath)
}

// CancelManifest moves a pending or in-progress manifest to the
// cancelled directory and updates its inline Status field. Returns
// an error if the manifest is not found in pending or in_progress.
//
// Order: rename first (atomic on same fs), then rewrite inline Status
// at the new location. The directory is authoritative per spec, so a
// crash between rename and status rewrite leaves a manifest in
// cancelled/ with a stale Status=pending field — readers still see
// it as cancelled and the inline field self-heals on the next save.
// The reverse order risks the worst outcome: a manifest in pending/
// with Status=cancelled, which contradicts the authoritative dir.
//
// Note: Manifest.String() prints the inline Status field. A concurrent
// reader that rendered a manifest between the rename and the inline
// rewrite would see the pre-cancel status. Acceptable because callers
// re-read after a successful CancelManifest return.
func (m *Manager) CancelManifest(id string) error {
	for _, fromStatus := range []Status{StatusPending, StatusInProgress} {
		fromPath := filepath.Join(m.dirForStatus(fromStatus), id+".json")
		if _, err := os.Stat(fromPath); os.IsNotExist(err) {
			continue
		}
		if err := m.MoveManifest(id, fromStatus, StatusCancelled); err != nil {
			return fmt.Errorf("move manifest %s to cancelled: %w", id, err)
		}
		toPath := filepath.Join(m.dirForStatus(StatusCancelled), id+".json")
		manifest, err := LoadManifest(toPath)
		if err != nil {
			return fmt.Errorf("reload manifest %s after move: %w", id, err)
		}
		manifest.Status = StatusCancelled
		if err := manifest.Save(toPath); err != nil {
			return fmt.Errorf("update inline status for %s: %w", id, err)
		}
		return nil
	}
	return fmt.Errorf("manifest %s not found in pending or in_progress", id)
}

// CreateManifest creates and saves a new manifest.
func (m *Manager) CreateManifest(description string, gmailIDs []string, filters Filters) (*Manifest, error) {
	manifest := NewManifest(description, gmailIDs)
	manifest.Filters = filters

	if err := m.SaveManifest(manifest); err != nil {
		return nil, err
	}

	return manifest, nil
}
