package reddit

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// Import retains archived originals. Repeating the directory safely fills missing
// keys and repairs replies and conversation statistics, including after failure.
func Import(ctx context.Context, st *store.Store, opts ImportOptions) (summary *ImportSummary, retErr error) {
	summary = &ImportSummary{}
	reader, err := openExport(ctx, opts.RootDir, opts.Me, summary)
	if err != nil {
		return summary, err
	}
	defer func() { retErr = errors.Join(retErr, reader.root.Close()) }()
	source, err := st.GetOrCreateSourceContext(ctx, "reddit", reader.owner)
	if err != nil {
		return summary, err
	}
	summary.SourceID = source.ID
	syncID, err := st.StartSyncContext(ctx, source.ID, "import-reddit")
	if err != nil {
		return summary, err
	}
	st = st.ScopedToSync(source.ID, syncID)
	defer func() {
		// Record partial runs and finalize committed rows even after cancellation.
		cleanup := context.WithoutCancel(ctx)
		retErr = errors.Join(retErr, st.ResolveDeferredMessageRepliesContext(cleanup, source.ID, "reddit_chat", batchSize), st.RecomputeConversationStatsContext(cleanup, source.ID))
		for _, f := range summary.Files {
			summary.Added += f.Added
			summary.Unchanged += f.Unchanged
			summary.Malformed += f.Malformed
			summary.MissingIDs += f.MissingIDs
			summary.UnknownDates += f.UnknownDates
			summary.UnknownSenders += f.UnknownSenders
		}
		retErr = errors.Join(retErr, st.UpdateSyncCheckpointContext(cleanup, syncID, &store.Checkpoint{
			MessagesProcessed: int64(summary.Added + summary.Unchanged), MessagesAdded: int64(summary.Added),
		}))
		if retErr != nil {
			retErr = errors.Join(retErr, st.FailSyncContext(cleanup, syncID, retErr.Error()))
		} else {
			retErr = st.CompleteSyncContext(cleanup, syncID, "")
		}
	}()
	for _, file := range reader.files {
		stats := &FileSummary{Filename: file.name}
		summary.Files = append(summary.Files, stats)
		batch := make([]csvRow, 0, batchSize)
		bytes := 0
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			err := importBatch(ctx, st, source.ID, reader.owner, file.kind, batch, stats)
			batch = batch[:0]
			bytes = 0
			return err
		}
		err := reader.rows(ctx, file, stats, func(row csvRow) error {
			id := row.Columns["id"]
			if file.kind == "chat" {
				id = row.Columns["message_id"]
			}
			if strings.TrimSpace(id) == "" {
				stats.MissingIDs++
				return nil
			}
			batch = append(batch, row)
			for _, v := range row.Columns {
				bytes += len(v)
			}
			if len(batch) >= batchSize || bytes >= 1<<20 {
				return flush()
			}
			return nil
		})
		// A parse failure does not discard complete records read before it.
		if err != nil {
			return summary, errors.Join(err, flush())
		}
		if err = flush(); err != nil {
			return summary, fmt.Errorf("reddit: %s: %w", file.name, err)
		}
	}
	return summary, nil
}

func rowKey(kind string, row csvRow) string {
	field := "id"
	if kind == "chat" {
		field = "message_id"
	}
	return kind + ":" + row.Columns[field]
}

func importBatch(ctx context.Context, st *store.Store, sourceID int64, owner, kind string, rows []csvRow, stats *FileSummary) error {
	keys := make([]string, len(rows))
	for i, row := range rows {
		keys[i] = rowKey(kind, row)
	}
	known, err := st.MessageExistsBatchContext(ctx, sourceID, keys)
	if err != nil {
		return err
	}
	for i, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := keys[i]
		c := row.Columns
		body, subject, sender, to, conv, title, convType := c["body"], "", owner, "", "", "", ""
		date := c["date"]
		switch kind {
		case "chat":
			convType = "chat"
			body = c["message"]
			sender = displayHandle(c["username"])
			date = c["created_at"]
			if c["channel_url"] != "" {
				conv = "chat:channel:" + c["channel_url"]
				title = c["channel_name"]
			} else {
				conv = "chat:item:" + c["message_id"]
			}
		case "pm":
			convType = "chat"
			sender = displayHandle(c["from"])
			to = displayHandle(c["to"])
			subject = c["subject"]
			title = subject
			conv = "pm:item:" + c["id"]
			if c["thread_id"] != "" {
				conv = "pm:thread:" + c["thread_id"]
			}
		case "post", "comment":
			conv = kind + ":item:" + c["id"]
			sub := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(c["subreddit"])), "r/")
			if sub != "" {
				title = "r/" + sub
			}
			if kind == "post" {
				subject = c["title"]
				if c["url"] != "" {
					if body != "" {
						body += "\n"
					}
					body += c["url"]
				}
			} else if c["media"] != "" {
				if body != "" {
					body += "\n"
				}
				body += c["media"]
			}
		}
		sent, err := time.Parse("2006-01-02 15:04:05 MST", date)
		if err != nil || !strings.HasSuffix(date, " UTC") {
			sent, err = time.Parse(time.RFC3339Nano, date)
		}
		if err != nil {
			sent = time.Time{}
			stats.UnknownDates++
		}
		if sender == "" {
			stats.UnknownSenders++
		}
		if _, ok := known[key]; ok {
			stats.Unchanged++
			continue
		}
		members := []store.ConversationParticipantRef{}
		recipients := []store.RecipientSet{}
		var senderID int64
		for _, person := range []struct{ handle, role string }{{sender, "from"}, {to, "to"}} {
			if person.handle == "" {
				continue
			}
			pid, err := st.EnsureParticipantByIdentifierContext(ctx, "reddit", strings.ToLower(person.handle), person.handle)
			if err != nil {
				return err
			}
			if person.role == "from" {
				senderID = pid
			}
			members = append(members, store.ConversationParticipantRef{ParticipantID: pid, Role: "member"})
			recipients = append(recipients, store.RecipientSet{Type: person.role, ParticipantIDs: []int64{pid}, DisplayNames: []string{person.handle}})
		}
		raw, err := json.Marshal(row, json.Deterministic(true))
		if err != nil {
			return err
		}
		var metadata *sql.NullString
		if kind == "chat" && c["thread_parent_message_id"] != "" {
			encoded, err := json.Marshal(map[string]string{"referenced_message_id": "chat:" + c["thread_parent_message_id"]})
			if err != nil {
				return err
			}
			metadata = &sql.NullString{String: string(encoded), Valid: true}
		}
		id, err := st.PersistMessageContext(ctx, &store.MessagePersistData{
			Message:      &store.Message{SourceID: sourceID, SourceMessageID: key, MessageType: "reddit_" + kind, SentAt: sql.NullTime{Time: sent, Valid: !sent.IsZero()}, SenderID: sql.NullInt64{Int64: senderID, Valid: senderID != 0}, IsFromMe: sender != "" && strings.EqualFold(sender, owner), Subject: sql.NullString{String: subject, Valid: subject != ""}, Snippet: sql.NullString{String: textutil.PrefixRunes(body, 200), Valid: body != ""}, SizeEstimate: int64(len(body))},
			Conversation: &store.ConversationPersistData{SourceConversationID: conv, ConversationType: convType, Title: title, Participants: members, PreserveExistingParticipants: true},
			BodyText:     sql.NullString{String: body, Valid: body != ""}, RawMIME: raw, RawFormat: "reddit_csv_row", Metadata: metadata, Recipients: recipients, FTS: &store.FTSDoc{Body: body, Subject: subject},
		})
		if err != nil {
			return err
		}
		known[key] = id
		stats.Added++
	}
	return nil
}
