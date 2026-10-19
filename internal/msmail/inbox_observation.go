package msmail

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func messageSnapshot(m DeltaMessage) inboxSnapshot {
	return inboxSnapshot{ID: m.ID, Categories: m.Categories, Read: m.IsRead, Parent: m.ParentFolderID, ETag: m.ETag}
}

// completeDelta keeps metadata from one response. A sparse opaque legacy delta
// may be completed by an independent exact-message lookup, never by joining
// fields from different responses or by substituting the folder being walked.
func (s *syncer) completeDelta(ctx context.Context, m DeltaMessage) DeltaMessage {
	if messageSnapshot(m).complete(m.ID) {
		return m
	}
	info, err := s.c.LookupMessage(ctx, m.ID)
	if err != nil || info.ID != m.ID {
		return m
	}
	fresh := deltaFromInfo(m.ID, info)
	fresh.archiveID = m.archiveID
	if fresh.ReceivedDateTime.IsZero() {
		fresh.ReceivedDateTime = m.ReceivedDateTime
	}
	return fresh
}

func deltaFromInfo(id string, info MessageInfo) DeltaMessage {
	m := DeltaMessage{ID: id, ReceivedDateTime: info.ReceivedDateTime, Categories: info.Categories, IsRead: info.IsRead, ParentFolderID: info.ParentFolderID, ETag: info.ETag}
	// Older importer responses may omit ID; their labels remain usable under the
	// existing lookup contract, but they cannot produce actionable native state.
	if info.ID != id {
		m.IsRead = nil
		m.ETag = ""
	}
	return m
}

func (s *syncer) observeMessage(ctx context.Context, m DeltaMessage, messageID, legacyFolder int64) error {
	target := inboxcontrol.Target{SourceID: s.sourceID, SourceType: SourceType, SourceIdentifier: s.opts.Email, AccountID: s.opts.Email, Scope: inboxcontrol.ScopeMessage, ItemID: messageID, ProviderID: m.ID}
	state := inboxcontrol.State{Target: target, ObservedAt: time.Now().UTC()}
	var folder *int64
	if label, ok := s.labels[m.ParentFolderID]; ok {
		folder = &label
	} else if m.ParentFolderID == "" && legacyFolder != 0 {
		folder = &legacyFolder
	}
	if messageSnapshot(m).complete(m.ID) && folder != nil && s.inbox != "" {
		state.Tags = append([]string{}, (*m.Categories)...)
		state.Read = m.IsRead
		state.Inbox = new(m.ParentFolderID == s.inbox)
		state.Location = m.ParentFolderID
		state.Revision = m.ETag
	}
	changed, err := s.st.ObserveMicrosoftMailStateContext(ctx, state, folder, m.Categories)
	if changed {
		s.sum.Moved++
	}
	return err
}
