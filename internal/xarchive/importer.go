package xarchive

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

const SourceType = "x_archive"
const MessageType = "x_dm"

type ImportOptions struct{ RootDir, Me, AttachmentsDir string }
type ImportSummary struct {
	SourceID                                                                       int64
	MessagesAdded, MessagesSkipped, InvalidRecords, EventsSkipped                  int64
	DirectConversations, GroupConversations, AttachmentsMissing, AttachmentsStored int
}
type message struct {
	ID        string   `json:"id"`
	Sender    string   `json:"senderId"`
	Recipient string   `json:"recipientId"`
	Text      string   `json:"text"`
	CreatedAt string   `json:"createdAt"`
	Media     []string `json:"mediaUrls"`
}
type event struct {
	Message *message `json:"messageCreate"`
	Join    *struct {
		Participants []string `json:"participantsSnapshot"`
	} `json:"joinConversation"`
}

type provenance struct {
	Filename     string         `json:"source_file"`
	Conversation string         `json:"conversation_id"`
	Owner        string         `json:"owner_id"`
	Account      jsontext.Value `json:"account,omitempty"`
	Membership   jsontext.Value `json:"observed_membership_event,omitempty"`
}

// Import replays supplied local files, preserving first-imported messages and
// repairing only attachment slots that still have no archived bytes.
func Import(ctx context.Context, st *store.Store, opts ImportOptions) (summary *ImportSummary, retErr error) {
	summary = &ImportSummary{}
	a, err := openArchive(opts.RootDir)
	if err != nil {
		return summary, err
	}
	defer func() { retErr = errors.Join(retErr, a.Close()) }()
	if err := a.readAccount(ctx); err != nil {
		return summary, err
	}
	owner := a.owner
	if opts.Me != "" {
		if !validID(opts.Me) {
			return summary, errors.New("xarchive: --me must be a positive numeric X account ID")
		}
		if owner != "" && owner != opts.Me {
			return summary, errors.New("xarchive: --me conflicts with account.js")
		}
		owner = opts.Me
	}
	if owner == "" {
		return summary, errors.New("xarchive: account.js missing; pass --me <numeric-id>")
	}
	source, err := st.GetOrCreateSourceContext(ctx, SourceType, owner)
	if err != nil {
		return summary, err
	}
	summary.SourceID = source.ID
	syncID, err := st.StartSyncContext(ctx, source.ID, "import-x")
	if err != nil {
		return summary, err
	}
	st = st.ScopedToSync(source.ID, syncID)
	defer func() {
		// Record the final sync state and statistics even after an interrupted import.
		cleanup := context.WithoutCancel(ctx)
		retErr = errors.Join(retErr, st.RecomputeConversationStatsContext(cleanup, source.ID))
		if retErr != nil {
			retErr = errors.Join(retErr, st.FailSyncContext(cleanup, syncID, retErr.Error()))
		} else {
			retErr = st.CompleteSyncContext(cleanup, syncID, "")
		}
	}()
	resolve := func(id string) (int64, error) {
		if id == "" {
			return 0, nil
		}
		name := ""
		if id == owner {
			name = a.name
		}
		return st.EnsureParticipantByIdentifierContext(ctx, "x_user", id, name)
	}
	if _, err := resolve(owner); err != nil {
		return summary, err
	}
	retErr = a.events(ctx, func(rec record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		kind := "direct_chat"
		if rec.Group {
			kind = "group_chat"
		}
		convID, err := st.EnsureConversationWithTypeContext(ctx, source.ID, rec.Peer, kind, "")
		if err != nil {
			return err
		}
		if rec.Raw == nil {
			if rec.Group {
				summary.GroupConversations++
			} else {
				summary.DirectConversations++
			}
			return nil
		}
		var e event
		if err := json.Unmarshal(rec.Raw, &e); err != nil {
			summary.InvalidRecords++
			return nil
		}
		prov := provenance{Filename: rec.Filename, Conversation: rec.Peer, Owner: owner, Account: a.account}
		if e.Join != nil && rec.Group {
			for _, id := range e.Join.Participants {
				if !validID(id) {
					summary.InvalidRecords++
					continue
				}
				pid, err := resolve(id)
				if err != nil {
					return err
				}
				if err := st.EnsureConversationParticipantContext(ctx, convID, pid, "member"); err != nil {
					return err
				}
			}
			// This is one observed snapshot, not a complete or chronologically final roster.
			prov.Membership = rec.Raw
			encoded, err := json.Marshal(prov)
			if err != nil {
				return err
			}
			if err := st.SetConversationMetadataContext(ctx, convID, sql.NullString{String: string(encoded), Valid: true}); err != nil {
				return err
			}
			summary.EventsSkipped++
			return nil
		}
		if e.Message == nil {
			summary.EventsSkipped++
			return nil
		}
		m := e.Message
		if !validID(m.ID) || (m.Sender != "" && !validID(m.Sender)) || (m.Recipient != "" && !validID(m.Recipient)) {
			summary.InvalidRecords++
			return nil
		}
		var sent time.Time
		if m.CreatedAt != "" {
			// Unknown or malformed times stay unknown; raw JSON retains the original string.
			sent, _ = time.Parse(time.RFC3339Nano, m.CreatedAt)
		}
		key := rec.Peer + ":" + m.ID
		existing, err := st.MessageExistsBatchContext(ctx, source.ID, []string{key})
		if err != nil {
			return err
		}
		id := existing[key]
		writes, err := mediaWrites(ctx, st, a.root, id, m, rec.Group, opts.AttachmentsDir, summary)
		if err != nil {
			return err
		}
		if id != 0 {
			for _, write := range writes {
				if err := st.UpsertAttachmentRecordWithStats(ctx, id, write, true); err != nil {
					return err
				}
			}
			summary.MessagesSkipped++
			return nil
		}
		members := []store.ConversationParticipantRef{}
		recipients := []store.RecipientSet{}
		var sender int64
		for _, entry := range []struct{ id, role string }{{m.Sender, "from"}, {m.Recipient, "to"}} {
			if entry.id == "" {
				continue
			}
			pid, err := resolve(entry.id)
			if err != nil {
				return err
			}
			if entry.role == "from" {
				sender = pid
			}
			members = append(members, store.ConversationParticipantRef{ParticipantID: pid, Role: "member"})
			recipients = append(recipients, store.RecipientSet{Type: entry.role, ParticipantIDs: []int64{pid}})
		}
		encoded, err := json.Marshal(prov)
		if err != nil {
			return err
		}
		var replacement *[]store.AttachmentWrite
		if len(writes) > 0 {
			replacement = &writes
		}
		_, err = st.PersistMessageContext(ctx, &store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: key, MessageType: MessageType, SentAt: sql.NullTime{Time: sent, Valid: !sent.IsZero()}, SenderID: sql.NullInt64{Int64: sender, Valid: sender != 0}, IsFromMe: m.Sender == owner, Snippet: sql.NullString{String: textutil.PrefixRunes(m.Text, 200), Valid: m.Text != ""}, SizeEstimate: int64(len(m.Text))},
			Conversation: &store.ConversationPersistData{SourceConversationID: rec.Peer, ConversationType: kind, Participants: members, PreserveExistingParticipants: true},
			RawMIME:      rec.Raw, RawFormat: "x_dm_json", BodyText: sql.NullString{String: m.Text, Valid: m.Text != ""}, Metadata: &sql.NullString{String: string(encoded), Valid: true}, Recipients: recipients, MIMEAttachmentReplacement: replacement, FTS: &store.FTSDoc{Body: m.Text},
		})
		if err != nil {
			return fmt.Errorf("xarchive: persist message: %w", err)
		}
		summary.MessagesAdded++
		return nil
	})
	return summary, retErr
}
