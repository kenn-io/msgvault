package agentdemo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const ManifestFile = "agent-demo.manifest.json"

type Owner struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Phone    string `json:"phone"`
	Timezone string `json:"timezone"`
}
type Source struct {
	ID         int64  `json:"id"`
	SourceType string `json:"source_type"`
	Identifier string `json:"identifier"`
	Family     string `json:"family"`
}
type Person struct {
	ID             int64    `json:"id"`
	ParticipantIDs []int64  `json:"participant_ids"`
	Identities     []string `json:"identities"`
}
type Participant struct {
	ID       int64  `json:"id"`
	Identity string `json:"identity"`
}
type Conversation struct {
	ID               int64  `json:"id"`
	Source           string `json:"source"`
	ConversationType string `json:"conversation_type"`
}
type Attachment struct {
	ContentHash    string   `json:"content_hash"`
	MessageHandles []string `json:"message_handles"`
}

type MeetingActionItem struct {
	Ordinal  int    `json:"ordinal"`
	Title    string `json:"title"`
	Assignee string `json:"assignee"`
}
type Meeting struct {
	MessageID   int64               `json:"message_id"`
	ActionItems []MeetingActionItem `json:"action_items"`
}
type Operation struct {
	Kind  string `json:"kind"`
	State string `json:"state"`
}

// Manifest names the archive rows and declares expected feature answers.
type Manifest struct {
	Scenario        string                  `json:"scenario"`
	ScenarioVersion int                     `json:"scenario_version"`
	Seed            uint64                  `json:"seed"`
	AsOf            time.Time               `json:"as_of"`
	Owner           Owner                   `json:"owner"`
	Sources         map[string]Source       `json:"sources"`
	Persons         map[string]Person       `json:"persons"`
	Participants    map[string]Participant  `json:"participants"`
	Conversations   map[string]Conversation `json:"conversations"`
	Messages        map[string]int64        `json:"messages"`
	Attachments     map[string]Attachment   `json:"attachments"`
	Meetings        map[string]Meeting      `json:"meetings"`
	CalendarEvents  map[string]int64        `json:"calendar_events"`
	Operations      map[string]Operation    `json:"operations"`
	Collections     map[string]int64        `json:"collections"`
	SavedViews      map[string]int64        `json:"saved_views"`
	Expect          map[string]any          `json:"expect"`
	Canaries        []string                `json:"canaries"`
	Digest          string                  `json:"digest"`
}

func (m *Manifest) Write(dir string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err = os.WriteFile(filepath.Join(dir, ManifestFile), append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}
func Load(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err = json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}
