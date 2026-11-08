package github

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"
)

// syncState is how far a GitHub source's discovery has come and which
// threads it found but has not archived yet. It is stored as the source's
// sync cursor. Discovery cursors advance in the same write that queues the
// threads they found, so a thread found is never lost to an interrupted or
// rate-limited run.
type syncState struct {
	// Owners is the owner filter the cursors were reached under.
	Owners        string             `json:"owners,omitempty"`
	Notifications notificationCursor `json:"notifications,omitzero"`
	// IssuesThrough and DiscussionsThrough are the latest updates the
	// involvement searches have covered.
	IssuesThrough      time.Time `json:"issues_through,omitzero"`
	DiscussionsThrough time.Time `json:"discussions_through,omitzero"`
	// Pending are the threads found and not yet archived, newest first.
	Pending []Thread `json:"pending,omitempty"`
}

type notificationCursor struct {
	// LastModified is the list's Last-Modified header, sent back as
	// If-Modified-Since.
	LastModified string `json:"last_modified,omitempty"`
	// Since is the latest thread update the list has shown.
	Since time.Time `json:"since,omitzero"`
}

func decodeState(cursor sql.NullString) (syncState, error) {
	var state syncState
	if !cursor.Valid || strings.TrimSpace(cursor.String) == "" {
		return state, nil
	}
	if err := json.Unmarshal([]byte(cursor.String), &state); err != nil {
		return syncState{}, fmt.Errorf("decode GitHub sync state: %w", err)
	}
	return state, nil
}

func (s syncState) encode() (string, error) {
	data, err := json.Marshal(s, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("encode GitHub sync state: %w", err)
	}
	return string(data), nil
}

// restartDiscovery forgets how far discovery came, keeping the queue.
func (s *syncState) restartDiscovery(owners string) {
	s.Owners = owners
	s.Notifications = notificationCursor{}
	s.IssuesThrough = time.Time{}
	s.DiscussionsThrough = time.Time{}
}
