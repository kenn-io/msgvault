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
		retErr = errors.Join(retErr, st.RecomputeConversationStatsContext(cleanup, source.ID),
			st.UpdateSyncCheckpointContext(cleanup, syncID, &store.Checkpoint{MessagesProcessed: summary.MessagesAdded + int64(summary.Duplicates+summary.Unsupported), MessagesAdded: summary.MessagesAdded}))
		if retErr != nil {
			retErr = errors.Join(retErr, st.FailSyncContext(cleanup, syncID, retErr.Error()))
		} else {
			retErr = st.CompleteSyncContext(cleanup, syncID, "")
		}
	}()
	if reader.profilePath != "" {
		config := map[string]jsontext.Value{}
		if source.SyncConfig.Valid {
			if err := json.Unmarshal([]byte(source.SyncConfig.String), &config); err != nil {
				return summary, err
			}
		}
		config["owner_profile_path"], err = json.Marshal(reader.profilePath)
		if err != nil {
			return summary, err
		}
		raw, err := json.Marshal(config)
		if err != nil {
			return summary, err
		}
		if err := st.UpdateSourceSyncConfig(ctx, source.ID, string(raw)); err != nil {
			return summary, err
		}
	}
	resolve := func(p person) (int64, error) {
		email, _ := NormalizeOwner(p.Email)
		if email == "" {
			return 0, nil
		}
		id, ensureErr := st.EnsureParticipantContext(ctx, email, p.Name, strings.SplitN(email, "@", 2)[1])
		if ensureErr != nil {
			return 0, ensureErr
		}
		_, updateErr := st.UpdateParticipantDisplayNameByEmail(ctx, email, p.Name)
		return id, updateErr
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
	preserveKind := strings.HasPrefix(group, "DM ") && len(info.Members) < 2
	conversationID, err := st.EnsureConversationWithTypePolicyContext(ctx, sourceID, group, kind, info.Name, preserveKind)
	if err != nil {
		return false, err
	}
	if len(groupPaths)+len(paths) > 0 {
		prior, err := st.GetConversationMetadataContext(ctx, conversationID)
		if err != nil {
			return false, err
		}
		metadata, err := mergeContainerEvidence(prior, "group_files", append(groupPaths, paths...))
		if err != nil {
			return false, err
		}
		if err := st.SetConversationMetadataContext(ctx, conversationID, metadata); err != nil {
			return false, err
		}
	}
	summary.Conversations++
	for _, member := range info.Members {
		id, err := resolve(member)
		if err != nil {
			return false, err
		}
		if id == 0 {
			summary.UnknownMembers++
			continue
		}
		if err := st.EnsureConversationParticipantContext(ctx, conversationID, id, "member"); err != nil {
			return false, err
		}
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
	existing, err := st.StoredMessagesContext(ctx, sourceID, "google_chat_json", ids)
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
			summary.Unsupported++
			continue
		}
		seen[m.ID] = record
		stored := existing[m.ID]
		id := stored.ID
		if id != 0 {
			if stored.ConversationID != conversationID {
				summary.Unsupported++
				continue
			}
			if len(stored.Raw) == 0 {
				return false, fmt.Errorf("googlechat: unsupported or unreadable stored raw format for %q", m.ID)
			}
			// Repair the archived slots using their original provider references.
			// A later edit/reordering must not attach a different file by index.
			original := stored.Raw
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
		var refs []store.ConversationParticipantRef
		var recipients []store.RecipientSet
		if sender != 0 {
			refs = append(refs, store.ConversationParticipantRef{ParticipantID: sender, Role: "member"})
			recipients = append(recipients, store.RecipientSet{Type: "from", ParticipantIDs: []int64{sender}, DisplayNames: []string{m.Creator.Name}})
		}
		metadata, err := json.Marshal(struct {
			Group   string `json:"group_id"`
			Topic   string `json:"topic_id"`
			Created string `json:"created_date"`
		}{group, m.Topic, m.Created})
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
			Conversation: &store.ConversationPersistData{SourceConversationID: group, ConversationType: kind, Title: info.Name, Participants: refs, PreserveExistingType: preserveKind, PreserveExistingParticipants: true},
			RawMIME:      record, RawFormat: "google_chat_json", BodyText: sql.NullString{String: m.Text, Valid: m.Text != ""}, Metadata: &sql.NullString{String: string(metadata), Valid: true}, Recipients: recipients, MIMEAttachmentReplacement: replacement, FTS: &store.FTSDoc{Body: m.Text, FromAddr: email},
		})
		if err != nil {
			return false, fmt.Errorf("googlechat: persist message: %w", err)
		}
		existing[m.ID] = store.StoredMessage{ID: id, ConversationID: conversationID, Raw: record}
		summary.MessagesAdded++
	}
	return len(envelope.Messages) > 0, nil
}

// Raw container snapshots live once per source/conversation. Distinct snapshots
// remain available when a later export changes a file or omits it entirely.
func mergeContainerEvidence(prior sql.NullString, key string, incoming []provenance) (sql.NullString, error) {
	metadata := map[string]jsontext.Value{}
	if prior.Valid {
		if err := json.Unmarshal([]byte(prior.String), &metadata); err != nil {
			return prior, err
		}
	}
	var files []provenance
	if raw := metadata[key]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &files); err != nil {
			return prior, err
		}
	}
	for _, file := range incoming {
		found := false
		for _, previous := range files {
			if previous.Path == file.Path && previous.Data == file.Data {
				found = true
				break
			}
		}
		if !found {
			files = append(files, file)
		}
	}
	raw, err := json.Marshal(files)
	if err != nil {
		return prior, err
	}
	metadata[key] = raw
	raw, err = json.Marshal(metadata)
	return sql.NullString{String: string(raw), Valid: err == nil}, err
}
