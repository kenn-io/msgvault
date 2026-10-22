// Package zoomcalls converts Zoom source evidence without inferring attendance.
package zoomcalls

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/callhistory"
)

type MeetingRecord struct {
	UUID   string
	Roster []byte
}

const MaxRosterBytes = callhistory.MaxRawBytes - (16 << 10)

type participant struct {
	ID     string    `json:"id"`
	UserID string    `json:"user_id"`
	Status string    `json:"status"`
	Join   time.Time `json:"join_time"`
	Leave  time.Time `json:"leave_time"`
}
type rosterPage struct {
	Participants []participant `json:"participants"`
	Next         string        `json:"next_page_token"`
	Total        int           `json:"total_records"`
}

// Meeting returns nil for complete evidence without confirmed coattendance.
// Bindings use stable Zoom IDs or "<uuid>/user/<user_id>" for anonymous rows.
//
//nolint:nilnil // Complete evidence without qualifying coattendance produces no observation.
func Meeting(account, owner, uuid string, raw []byte, bindings map[string]int64) (*callhistory.Observation, error) {
	if !validNativeID(account) || !validNativeID(owner) || !isExactInstanceUUID(uuid) || len(raw) > MaxRosterBytes || len(bindings) > 100 {
		return nil, errors.New("invalid Zoom owner, UUID, roster size or bindings")
	}
	for native, id := range bindings {
		if native == "" || native == owner || id <= 0 {
			return nil, errors.New("invalid Zoom participant binding")
		}
	}
	raw = bytes.TrimSpace(raw)
	var pages []rosterPage
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &pages); err != nil {
			return nil, errors.New("invalid Zoom roster")
		}
	} else {
		var page rosterPage
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, errors.New("invalid Zoom roster")
		}
		pages = []rosterPage{page}
	}
	if len(pages) == 0 || len(pages) > 100 {
		return nil, errors.New("invalid Zoom roster pages")
	}
	groups := map[string][]participant{}
	aliases := map[string]string{}
	tokens := map[string]bool{}
	count := 0
	for i, page := range pages {
		if page.Participants == nil || (i == len(pages)-1 && page.Next != "") ||
			(i < len(pages)-1 && (page.Next == "" || tokens[page.Next])) {
			return nil, errors.New("incomplete Zoom roster pagination")
		}
		tokens[page.Next] = true
		count += len(page.Participants)
		for _, p := range page.Participants {
			if p.Status != "in_meeting" {
				continue
			}
			if p.Join.IsZero() || p.Leave.IsZero() || p.Leave.Before(p.Join) {
				return nil, errors.New("invalid Zoom attendance interval")
			}
			id := p.ID
			if id == "" && p.UserID != "" {
				id = uuid + "/user/" + p.UserID
			}
			if p.UserID != "" {
				if previous, ok := aliases[p.UserID]; ok && previous != id {
					return nil, errors.New("ambiguous Zoom participant identity")
				}
				aliases[p.UserID] = id
			}
			if id != "" && p.Status == "in_meeting" && p.Join.Before(p.Leave) {
				groups[id] = append(groups[id], p)
			}
		}
	}
	for _, page := range pages {
		if page.Total != 0 && page.Total != count {
			return nil, errors.New("inconsistent or incomplete Zoom roster total")
		}
	}
	ownerRows := groups[owner]
	if len(ownerRows) == 0 {
		return nil, nil
	}
	start, end := ownerRows[0].Join, ownerRows[0].Leave
	for _, row := range ownerRows {
		if row.Join.Before(start) {
			start = row.Join
		}
		if row.Leave.After(end) {
			end = row.Leave
		}
	}
	o := &callhistory.Observation{
		Version: 1, Provider: "zoom_meetings", Account: AccountNamespace(account, owner), NativeCallID: uuid,
		Kind: "call", Outcome: "answered", Direction: "unknown", OwnerParticipation: "joined",
		OccurredAt: start.UTC(), EndedAt: &end, Precision: "instant", RecordID: uuid, Raw: jsontext.Value(raw)}
	joined := false
	ids := map[int64]bool{}
	for native, rows := range groups {
		if native == owner {
			continue
		}
		overlap := false
		for _, a := range ownerRows {
			for _, b := range rows {
				if a.Join.Before(b.Leave) && b.Join.Before(a.Leave) {
					overlap = true
				}
			}
		}
		if !overlap {
			continue
		}
		joined = true
		if id, ok := bindings[native]; ok {
			if id <= 0 {
				return nil, errors.New("invalid Zoom participant binding")
			}
			ids[id] = true
		}
	}
	for native, id := range bindings {
		if native == "" || native == owner || id <= 0 {
			return nil, errors.New("invalid Zoom participant binding")
		}
	}
	if !joined {
		return nil, nil
	}
	for id := range ids {
		o.Participants = append(o.Participants, callhistory.Participant{ID: id, Participation: "joined"})
	}
	sort.Slice(o.Participants, func(i, j int) bool { return o.Participants[i].ID < o.Participants[j].ID })
	evidence, err := RosterEvidence(raw)
	if err != nil {
		return nil, err
	}
	o.Raw = evidence
	if err := o.Validate(); err != nil {
		return nil, fmt.Errorf("zoom meeting: %w", err)
	}
	return o, nil
}

// AccountNamespace keeps organizations and archive owners separate even when
// Zoom reuses an instance or call ID. Reuse the archive's length-framed hash.
func AccountNamespace(account, owner string) string {
	return (callhistory.Evidence{Provider: "zoom_account", Account: account, NativeCallID: owner}).CanonicalKey()
}
func validNativeID(id string) bool {
	return strings.TrimSpace(id) != "" && len(id) <= 256 && utf8.ValidString(id) && !strings.ContainsRune(id, 0)
}

func isExactInstanceUUID(uuid string) bool {
	return validNativeID(uuid) && strings.TrimSpace(uuid) == uuid && !isNumericZoomMeetingID(uuid)
}

func isNumericZoomMeetingID(id string) bool {
	return id != "" && strings.Trim(id, "0123456789") == ""
}

// RosterEvidence removes transient pagination tokens after the complete roster
// has been validated. Participant payloads remain intact.
func RosterEvidence(raw []byte) (jsontext.Value, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > MaxRosterBytes {
		return nil, errors.New("invalid Zoom roster evidence size")
	}
	var value any
	if raw[0] == '[' {
		var pages []map[string]jsontext.Value
		if err := json.Unmarshal(raw, &pages); err != nil || len(pages) == 0 {
			return nil, errors.New("invalid Zoom roster evidence")
		}
		for _, page := range pages {
			delete(page, "next_page_token")
		}
		value = pages
	} else {
		var page map[string]jsontext.Value
		if err := json.Unmarshal(raw, &page); err != nil || page == nil {
			return nil, errors.New("invalid Zoom roster evidence")
		}
		delete(page, "next_page_token")
		value = page
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode Zoom roster evidence: %w", err)
	}
	return encoded, nil
}
