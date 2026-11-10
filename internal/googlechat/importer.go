package googlechat

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// Import replays explicit parts, preserving first-imported snapshots and
// enriching only missing media slots. A failed run can be replayed in full.
func Import(ctx context.Context, st *store.Store, opts ImportOptions) (summary *ImportSummary, retErr error) {
	summary = &ImportSummary{}
	start := time.Now()
	defer func() { summary.Duration = time.Since(start) }()
	owner, err := NormalizeOwner(opts.Me)
	if err != nil {
		return summary, err
	}
	reader, err := openExport(ctx, opts.Roots, owner)
	if err != nil {
		return summary, err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	source, err := st.GetOrCreateSourceContext(ctx, SourceType, owner)
	if err != nil {
		return summary, err
	}
	summary.SourceID = source.ID
	syncID, err := st.StartSyncContext(ctx, source.ID, "import-google-chat")
	if err != nil {
		return summary, err
	}
	st = st.ScopedToSync(source.ID, syncID)
	defer func() {
		// Committed messages need accurate stats and a terminal run status after
		// cancellation; neither cleanup step reads more provider data.
		cleanup := context.WithoutCancel(ctx)
		retErr = errors.Join(retErr, st.RecomputeConversationStatsContext(cleanup, source.ID))
		if retErr != nil {
			retErr = errors.Join(retErr, st.FailSyncContext(cleanup, syncID, retErr.Error()))
		} else {
			retErr = st.CompleteSyncContext(cleanup, syncID, "")
		}
	}()
	config, err := json.Marshal(struct {
		Roots    []string     `json:"supplied_roots"`
		Profiles []provenance `json:"owner_profiles"`
	}{opts.Roots, reader.users})
	if err != nil {
		return summary, err
	}
	if err := st.UpdateSourceSyncConfig(ctx, source.ID, string(config)); err != nil {
		return summary, err
	}
	resolve := func(p person) (int64, error) {
		email, err := NormalizeOwner(p.Email)
		if err != nil {
			return 0, nil
		}
		return st.EnsureParticipantContext(ctx, email, p.Name, strings.SplitN(email, "@", 2)[1])
	}
	supplied := map[string]bool{}
	for _, group := range reader.groupIDs() {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		hasMessages, err := importGroup(ctx, st, reader, group, owner, source.ID, resolve, opts, summary)
		if err != nil {
			return summary, err
		}
		supplied[group] = hasMessages
		if opts.Logger != nil {
			opts.Logger.Info("googlechat: supplied conversation processed")
		}
	}
	for group := range reader.memberships {
		if !supplied[group] {
			summary.MembershipWithoutMessages++
		}
	}
	return summary, nil
}

func importGroup(ctx context.Context, st *store.Store, reader *exportReader, group, owner string, sourceID int64, resolve func(person) (int64, error), opts ImportOptions, summary *ImportSummary) (bool, error) {
	raw, paths, err := reader.groupFiles(ctx, group, "messages.json")
	if err != nil {
		return false, err
	}
	groupRaw, groupPaths, err := reader.groupFiles(ctx, group, "group_info.json")
	if err != nil {
		return false, err
	}
	var info groupInfo
	if groupRaw != nil {
		if err := json.Unmarshal(groupRaw, &info); err != nil {
			return false, fmt.Errorf("googlechat: group JSON: %w", err)
		}
	}
	kind := "group_chat"
	if strings.HasPrefix(group, "DM ") {
		// The folder prefix alone does not establish a two-person roster.
		switch len(info.Members) {
		case 0, 1:
			kind = "chat"
		case 2:
			kind = "direct_chat"
		}
	}
	conversationID, err := st.EnsureConversationWithTypeContext(ctx, sourceID, group, kind, info.Name)
	if err != nil {
		return false, err
	}
	summary.Conversations++
	var members []store.ConversationParticipantRef
	for _, member := range info.Members {
		id, err := resolve(member)
		if err != nil {
			return false, err
		}
		if id == 0 {
			continue
		}
		if err := st.EnsureConversationParticipantContext(ctx, conversationID, id, "member"); err != nil {
			return false, err
		}
		members = append(members, store.ConversationParticipantRef{ParticipantID: id, Role: "member"})
	}
	if raw == nil {
		return false, nil
	}
	var envelope struct {
		Messages []jsontext.Value `json:"messages"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return false, fmt.Errorf("googlechat: messages JSON: %w", err)
	}
	if envelope.Messages == nil {
		return false, errors.New("googlechat: messages JSON needs a messages array")
	}
	summary.MessagesSupplied += len(envelope.Messages)
	// Batch lookup stays bounded by one JSON file, without archive-wide ID maps.
	var ids []string
	for _, record := range envelope.Messages {
		var m message
		if json.Unmarshal(record, &m) == nil && m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	existing, err := st.MessageExistsBatchContext(ctx, sourceID, ids)
	if err != nil {
		return false, err
	}
	seen := map[string]jsontext.Value{}
	for _, record := range envelope.Messages {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		var m message
		if err := json.Unmarshal(record, &m); err != nil || m.ID == "" {
			summary.Unsupported++
			continue
		}
		if previous, ok := seen[m.ID]; ok && !bytes.Equal(previous, record) {
			return false, fmt.Errorf("googlechat: conflicting message ID %q", m.ID)
		}
		seen[m.ID] = record
		id := existing[m.ID]
		if id != 0 {
			// Repair the archived slots using their original provider references.
			// A later edit/reordering must not attach a different file by index.
			original, err := st.GetMessageRawContext(ctx, id)
			if err != nil {
				return false, err
			}
			var archived message
			if err := json.Unmarshal(original, &archived); err != nil {
				return false, fmt.Errorf("googlechat: read archived media references: %w", err)
			}
			writes, err := mediaWrites(ctx, st, reader, group, id, &archived, opts, summary)
			if err != nil {
				return false, err
			}
			for _, write := range writes {
				if err := st.UpsertAttachmentRecordWithStats(ctx, id, write, true); err != nil {
					return false, err
				}
			}
			summary.Duplicates++
			continue
		}
		sender, err := resolve(m.Creator)
		if err != nil {
			return false, err
		}
		if sender == 0 {
			summary.UnknownSenders++
		}
		sent := parseDate(m.Created)
		if sent.IsZero() {
			summary.DatesUnparsed++
		}
		email, _ := NormalizeOwner(m.Creator.Email)
		refs := append([]store.ConversationParticipantRef{}, members...)
		var recipients []store.RecipientSet
		if sender != 0 {
			refs = append(refs, store.ConversationParticipantRef{ParticipantID: sender, Role: "member"})
			recipients = append(recipients, store.RecipientSet{Type: "from", ParticipantIDs: []int64{sender}, DisplayNames: []string{m.Creator.Name}})
		}
		// Only the observed roster supplies recipients. --me scopes the archive;
		// it does not assert membership in a conversation with no roster evidence.
		var to []int64
		for _, member := range members {
			if member.ParticipantID != sender {
				to = append(to, member.ParticipantID)
			}
		}
		if len(to) > 0 {
			recipients = append(recipients, store.RecipientSet{Type: "to", ParticipantIDs: to})
		}
		metadata, err := json.Marshal(struct {
			Group    string         `json:"group_id"`
			Topic    string         `json:"topic_id"`
			Created  string         `json:"created_date"`
			Provider jsontext.Value `json:"provider_record"`
			Files    []provenance   `json:"message_files"`
			Groups   []provenance   `json:"group_files"`
			Profiles []provenance   `json:"owner_profiles"`
		}{group, m.Topic, m.Created, record, paths, groupPaths, reader.users})
		if err != nil {
			return false, err
		}
		writes, err := mediaWrites(ctx, st, reader, group, 0, &m, opts, summary)
		if err != nil {
			return false, err
		}
		var replacement *[]store.AttachmentWrite
		if len(writes) > 0 {
			replacement = &writes
		}
		snippet := textutil.PrefixRunes(strings.TrimSpace(m.Text), 200)
		id, err = st.PersistMessageContext(ctx, &store.MessagePersistData{
			Message:      &store.Message{SourceID: sourceID, SourceMessageID: m.ID, MessageType: store.MessageTypeGoogleChat, SentAt: sql.NullTime{Time: sent, Valid: !sent.IsZero()}, SenderID: sql.NullInt64{Int64: sender, Valid: sender != 0}, IsFromMe: sender != 0 && email == owner, Snippet: sql.NullString{String: snippet, Valid: snippet != ""}, SizeEstimate: int64(len(record))},
			Conversation: &store.ConversationPersistData{SourceConversationID: group, ConversationType: kind, Title: info.Name, Participants: refs, PreserveExistingParticipants: true},
			RawMIME:      record, RawFormat: "google_chat_json", BodyText: sql.NullString{String: m.Text, Valid: m.Text != ""}, Metadata: &sql.NullString{String: string(metadata), Valid: true}, Recipients: recipients, MIMEAttachmentReplacement: replacement, FTS: &store.FTSDoc{Body: m.Text},
		})
		if err != nil {
			return false, fmt.Errorf("googlechat: persist message: %w", err)
		}
		existing[m.ID] = id
		summary.MessagesAdded++
	}
	return len(envelope.Messages) > 0, nil
}
