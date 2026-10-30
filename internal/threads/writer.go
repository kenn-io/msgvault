package threads

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// ItemOrigin identifies the endpoint provenance and requested conversation.
type ItemOrigin struct{ Kind, RootID string }

// Writer persists Threads snapshots without making provider or CDN requests.
type Writer struct {
	store            *store.Store
	sourceID         int64
	sourceIdentifier string
	now              func() time.Time
	// afterPersist is a package-private crash seam before edit/link repair.
	afterPersist func() error
}

func NewWriter(s *store.Store, sourceID int64, sourceIdentifier string) *Writer {
	return &Writer{store: s, sourceID: sourceID, sourceIdentifier: sourceIdentifier}
}

func (w *Writer) checkSource(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, _, err := tokenSourceIDs(w.sourceIdentifier); err != nil {
		return err
	}
	source, err := w.store.GetSourceByIDContext(ctx, w.sourceID)
	if err != nil {
		return fmt.Errorf("Threads writer source: %w", err)
	}
	if source.SourceType != "threads" || source.Identifier != w.sourceIdentifier {
		return errors.New("Threads writer source binding mismatch")
	}
	return nil
}

// Apply returns zero without error for a repost facade. Incomplete or
// contradictory items return an error so callers cannot checkpoint past them.
func (w *Writer) Apply(ctx context.Context, m Media, origin ItemOrigin) (int64, error) {
	if err := w.checkSource(ctx); err != nil {
		return 0, err
	}
	m.Timestamp = m.Timestamp.UTC()
	root, err := mediaRoot(m, origin)
	if err != nil {
		return 0, err
	}
	if m.RepostedPost != nil {
		return 0, nil
	}
	previous, exists, err := w.store.ThreadsArchiveMessageMetadataContext(ctx, w.sourceID, m.ID)
	if err != nil {
		return 0, fmt.Errorf("Threads archive metadata: %w", err)
	}
	if exists && previous.SourceConversationID.String != root {
		return 0, errors.New("Threads snapshot cannot change conversation")
	}
	e, err := decodeEvidence(previous.Metadata.String)
	if err != nil {
		return 0, err
	}
	// Once canonical provider data exists, local send completion only repairs
	// its side effects. It cannot replace the timestamp, text or evidence.
	if exists && origin.Kind == "sent_provisional" && !e.flag("sent_provisional") {
		if previous.SourceConversationID.String != root {
			return 0, errors.New("provisional Threads root mismatch")
		}
		parentID, err := w.validateParent(ctx, m.ID, root, e.text("parent_id"))
		if err != nil {
			return previous.ID, err
		}
		if _, _, err := w.resolveAuthor(ctx, m.ID, e, e.flag("author_owned"), false); err != nil {
			return previous.ID, err
		}
		return previous.ID, w.repair(ctx, previous.ID, parentID, e)
	}
	wasProvisional := e.flag("sent_provisional")
	oldHash := e.text("body_hash")
	if err := preserveOwnerClaim(e, m, origin); err != nil {
		return 0, err
	}
	e.mergeMedia(m)
	// Preserve nanoseconds independently of the backend timestamp precision.
	e.put("timestamp", m.Timestamp)
	// Content lives in the body/raw tables. Metadata feeds graph/root queries
	// and must not become a second copy of every message body.
	delete(e, "text")
	parent := e.text("parent_id")
	if m.RepliedTo != nil {
		parent = m.RepliedTo.ID
	} else if m.Present["replied_to"] {
		parent = ""
	}
	parentID, err := w.validateParent(ctx, m.ID, root, parent)
	if err != nil {
		return 0, err
	}
	owner, err := w.ownerEvidence(e, origin)
	if err != nil {
		return 0, err
	}
	body := ""
	if exists && m.Text == nil && !m.Present["text"] {
		body, _, err = w.store.GetMessageBodyAndRawContext(ctx, previous.ID)
		if err != nil {
			return 0, fmt.Errorf("Threads sparse body: %w", err)
		}
	} else if m.Text != nil {
		body = *m.Text
	}
	// A hidden snapshot with no text does not revoke archived content.
	if exists && body == "" && strings.EqualFold(e.text("hide_status"), "hidden") {
		body, _, err = w.store.GetMessageBodyAndRawContext(ctx, previous.ID)
		if err != nil {
			return 0, fmt.Errorf("Threads hidden body: %w", err)
		}
	}
	hash := bodyHash(body)
	if exists && !wasProvisional && origin.Kind != "sent_provisional" && oldHash != "" && oldHash != hash {
		e.put("text_changed", true)
	}
	e.put("body_hash", hash)
	e.put("root_id", root)
	if parent != "" {
		e.put("parent_id", parent)
		e.put("referenced_message_id", parent)
	} else {
		delete(e, "parent_id")
		delete(e, "referenced_message_id")
	}
	e.put("root_owned", e.flag("root_owned") || (origin.Kind == "own_posts" && root == m.ID && owner))
	e.put("author_owned", owner)
	e.put("sent_provisional", origin.Kind == "sent_provisional")
	participant, label, err := w.resolveAuthor(ctx, m.ID, e, owner, m.Username != nil || m.Present["username"])
	if err != nil {
		return 0, err
	}
	metadata, err := json.Marshal(e)
	if err != nil {
		return 0, fmt.Errorf("Threads snapshot metadata: %w", err)
	}
	raw := m.Raw
	if len(raw) == 0 {
		// Direct callers (including provisional sends) have no wire snapshot.
		wire := snapshotEvidence{}
		wire.mergeMedia(m)
		wire.put("id", m.ID)
		wire.put("timestamp", m.Timestamp)
		raw, err = json.Marshal(wire)
		if err != nil {
			return 0, err
		}
	}
	title := ""
	if root == m.ID {
		title = rootTitle(body)
	} else {
		_, rootExists, err := w.store.ThreadsArchiveMessageMetadataContext(ctx, w.sourceID, root)
		if err != nil {
			return 0, err
		}
		if !rootExists {
			title = partialTitle(root)
		}
	}
	msg := store.Message{SourceID: w.sourceID, SourceMessageID: m.ID, MessageType: "threads", SenderID: sql.NullInt64{Int64: participant, Valid: true}, IsFromMe: owner,
		SentAt: sql.NullTime{Time: m.Timestamp, Valid: true}, ReceivedAt: sql.NullTime{Time: m.Timestamp, Valid: true}, Snippet: sql.NullString{String: rootTitleSnippet(body), Valid: body != ""}, SizeEstimate: int64(len(body))}
	meta := sql.NullString{String: string(metadata), Valid: true}
	id, err := w.store.PersistMessageContext(ctx, &store.MessagePersistData{
		Message: &msg, Conversation: &store.ConversationPersistData{SourceConversationID: root, ConversationType: "channel", Title: title,
			Participants: []store.ConversationParticipantRef{{ParticipantID: participant, Role: "member"}}, PreserveExistingParticipants: true},
		Metadata: &meta, BodyText: sql.NullString{String: body, Valid: true}, RawMIME: raw, RawFormat: "threads_json",
		Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{participant}, DisplayNames: []string{label}}},
		FTS:        &store.FTSDoc{Body: body, FromAddr: label},
	})
	if err != nil {
		return 0, fmt.Errorf("persist Threads snapshot: %w", err)
	}
	if w.afterPersist != nil {
		if err := w.afterPersist(); err != nil {
			return id, err
		}
	}
	return id, w.repair(ctx, id, parentID, e)
}

func (w *Writer) repair(ctx context.Context, id, parentID int64, e snapshotEvidence) error {
	if e.flag("text_changed") {
		if err := w.store.SetMessageEditedContext(ctx, id); err != nil {
			return fmt.Errorf("repair Threads edit flag: %w", err)
		}
	}
	// The message upsert preserves an existing reply link. Reconcile it on
	// every replay, including an explicit clear or a now-missing parent.
	if parentID != 0 {
		if err := w.store.SetMessageReplyContext(ctx, id, parentID); err != nil {
			return fmt.Errorf("repair Threads parent: %w", err)
		}
	} else if err := w.store.ClearMessageRepliesContext(ctx, w.sourceID, []int64{id}); err != nil {
		return err
	}
	return nil
}

// ResolveParents repeats after committed pages and on resume. It validates
// native metadata before linking, including cycles through missing parents.
// Callers run this global deferred pass at explicit page/resume boundaries;
// Apply repairs only its current message and does not call this method.
func (w *Writer) ResolveParents(ctx context.Context) error {
	if err := w.checkSource(ctx); err != nil {
		return err
	}
	var after int64
	for {
		pending, err := w.store.ListUnresolvedMessageRepliesAfterContext(ctx, w.sourceID, "threads", after, 200)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		for _, reply := range pending {
			if err := ctx.Err(); err != nil {
				return err
			}
			after = reply.MessageID
			record, found, err := w.store.ThreadsArchiveMessageMetadataContext(ctx, w.sourceID, reply.SourceMessageID)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			e, err := decodeEvidence(record.Metadata.String)
			if err != nil {
				return err
			}
			parent := e.text("parent_id")
			root := e.text("root_id")
			if root != record.SourceConversationID.String {
				return errors.New("cross-conversation Threads reply metadata")
			}
			parentID, err := w.validateParent(ctx, reply.SourceMessageID, root, parent)
			if err != nil {
				return err
			}
			if parentID != 0 {
				if err := w.store.SetMessageReplyContext(ctx, record.ID, parentID); err != nil {
					return fmt.Errorf("link Threads parent: %w", err)
				}
			}
		}
	}
}
