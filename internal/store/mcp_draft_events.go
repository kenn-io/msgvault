package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type draftCreatorKey struct{}

const draftCreatorUnknown = "unknown"

// WithDraftCreator carries a trusted caller principal to a draft creation.
// Creation paths without authenticated context retain an unknown creator.
func WithDraftCreator(ctx context.Context, principal string) context.Context {
	if !validDraftCreator(principal) {
		principal = draftCreatorUnknown
	}
	return context.WithValue(ctx, draftCreatorKey{}, principal)
}

func validDraftCreator(principal string) bool {
	if principal == "owner" || principal == "session" || principal == draftCreatorUnknown {
		return true
	}
	if !strings.HasPrefix(principal, "agent:") || len(principal) == len("agent:") {
		return false
	}
	return !strings.ContainsFunc(principal, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

func draftCreatorSQL(ctx context.Context) any {
	principal, _ := ctx.Value(draftCreatorKey{}).(string)
	if principal == "" || principal == draftCreatorUnknown || !validDraftCreator(principal) {
		return nil
	}
	return principal
}

type draftEventSnapshot struct {
	source, conversation, message, revision int64
	creator                                 string
}

func (s *Store) draftEventSnapshotTx(ctx context.Context, tx *loggedTx, draftKind, draftID string) (snapshot draftEventSnapshot, err error) {
	var query string
	switch draftKind {
	case "gmail", "imap":
		query = `SELECT d.source_id,m.conversation_id,d.current_message_id,d.revision,COALESCE(d.created_by_principal,'unknown') FROM ` + draftKind + `_drafts d JOIN messages m ON m.id=d.current_message_id WHERE d.draft_id=?`
	case "beeper":
		query = `SELECT d.source_id,c.id,0,d.revision,COALESCE(d.created_by_principal,'unknown') FROM beeper_drafts d JOIN conversations c ON c.source_id=d.source_id AND c.source_conversation_id=d.chat_id WHERE d.draft_id=?`
	case "chat":
		query = `SELECT source_id,conversation_id,0,revision,COALESCE(created_by_principal,'unknown') FROM chat_drafts WHERE draft_id=?`
	default:
		return snapshot, fmt.Errorf("unsupported draft kind %q", draftKind)
	}
	err = tx.QueryRowContext(ctx, query, draftID).Scan(&snapshot.source, &snapshot.conversation, &snapshot.message, &snapshot.revision, &snapshot.creator)
	if err == nil && !validDraftCreator(snapshot.creator) {
		snapshot.creator = draftCreatorUnknown
	}
	return
}

func (s *Store) appendDraftEventTx(ctx context.Context, tx *loggedTx, draftKind, draftID, kind string) error {
	if !s.captureMCPEnabled() {
		return nil
	}
	snapshot, err := s.draftEventSnapshotTx(ctx, tx, draftKind, draftID)
	// Beeper drafts may refer to a chat that has never been archived.
	if errors.Is(err, sql.ErrNoRows) && draftKind == "beeper" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read draft event scope: %w", err)
	}
	return s.appendDraftSnapshotTx(ctx, tx, draftKind, draftID, kind, snapshot)
}

func (s *Store) appendDraftSnapshotTx(ctx context.Context, tx *loggedTx, draftKind, draftID, kind string, snapshot draftEventSnapshot) error {
	if !s.captureMCPEnabled() {
		return nil
	}
	changed := time.Now().UTC()
	payload := map[string]any{"kind": kind, "draft_id": draftID, "revision": snapshot.revision, "draft_kind": draftKind, "conversation_id": strconv.FormatInt(snapshot.conversation, 10), "source_id": strconv.FormatInt(snapshot.source, 10), "created_by": snapshot.creator, "changed_at": changed.Format(time.RFC3339Nano)}
	if snapshot.message > 0 {
		payload["message_id"] = strconv.FormatInt(snapshot.message, 10)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	live := s.WithIngestContext(IngestContext{Mode: IngestLive, ObservedAt: changed})
	return live.appendMCPEventTx(ctx, tx, MCPEvent{Family: "msgvault.draft_changed", Kind: kind, ScopeKind: "conversation", ScopeID: snapshot.conversation, ConversationID: snapshot.conversation, SourceID: snapshot.source, MessageID: snapshot.message, ItemKey: fmt.Sprintf("draft:%s:%d:%s", draftID, snapshot.revision, kind), OccurredAt: changed, Data: raw})
}
