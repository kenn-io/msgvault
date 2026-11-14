package store_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestFTSDiagnosticDocuments(t *testing.T) {
	for _, tc := range []struct {
		name, body, issue string
		change            func(*testing.T, *store.Store, int64)
	}{
		{name: "matching", body: "private synthetic orchard"},
		{name: "legitimately empty"},
		{name: "missing", issue: "missing", change: func(t *testing.T, st *store.Store, id int64) {
			t.Helper()
			query := `DELETE FROM messages_fts WHERE rowid = ?`
			if st.IsPostgreSQL() {
				query = `UPDATE messages SET search_fts = NULL WHERE id = ?`
			}
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(query), id)
			require.NoError(t, err)
		}},
		{name: "different indexed content", body: "private synthetic orchard", issue: "document_mismatch", change: func(t *testing.T, st *store.Store, id int64) {
			t.Helper()
			require.NoError(t, st.UpsertFTSContext(t.Context(), id, "", "stale mango", "", "", ""))
		}},
		{name: "persisted body changed", body: "private synthetic orchard", issue: "document_mismatch", change: func(t *testing.T, st *store.Store, id int64) {
			t.Helper()
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE message_bodies SET body_text = 'changed pear' WHERE message_id = ?`), id)
			require.NoError(t, err)
		}},
		{name: "input bound", body: strings.Repeat("x", 1_000_001)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, r := assert.New(t), require.New(t)
			f := storetest.New(t)
			id := f.CreateMessage("synthetic")
			r.NoError(f.Store.UpsertMessageBodyContext(t.Context(), id, sql.NullString{String: tc.body, Valid: true}, sql.NullString{}))
			r.NoError(f.Store.IndexStoredMessageContext(t.Context(), id))
			if tc.change != nil {
				tc.change(t, f.Store, id)
			}
			var before, after string
			query := `SELECT total_changes()`
			var args []any
			if f.Store.IsPostgreSQL() {
				query = `SELECT xmin::text FROM messages WHERE id = $1`
				args = []any{id}
			} else {
				f.Store.DB().SetMaxOpenConns(1)
			}
			r.NoError(f.Store.DB().QueryRowContext(t.Context(), query, args...).Scan(&before))
			got, err := f.Store.DiagnoseFTSContext(t.Context(), store.FTSDiagnosticOptions{})
			r.NoError(err)
			r.Len(got.Entries, 1)
			a.True(got.Complete)
			a.Equal(id, got.Entries[0].MessageID)
			if tc.name == "input bound" {
				a.Equal("incomplete", got.Outcome)
				a.Equal("input_limit", got.Entries[0].Reason)
				a.Equal(1, got.Inconclusive)
			} else if tc.issue != "" {
				a.Equal("defects", got.Outcome)
				a.Contains(got.Entries[0].Issues, tc.issue)
				a.Equal(1, got.Defects)
				// Backfill finds a missing entry, but does not detect wrong
				// contents in an entry that is already present.
				needs, err := f.Store.NeedsFTSBackfillContext(t.Context())
				r.NoError(err)
				a.Equal(tc.issue == "missing", needs)
			} else {
				a.Equal("ok", got.Outcome)
				a.Equal(1, got.Matched)
				a.Equal(tc.body == "", got.Entries[0].EmptyDocument)
			}
			data, err := json.Marshal(got)
			r.NoError(err)
			a.NotContains(string(data), "private synthetic")
			a.NotContains(string(data), "stale mango")
			r.NoError(f.Store.DB().QueryRowContext(t.Context(), query, args...).Scan(&after))
			a.Equal(before, after, "diagnosis performs no writes")
		})
	}
}

func TestFTSDiagnosticPagination(t *testing.T) {
	a, r := assert.New(t), require.New(t)
	f := storetest.New(t)
	ids := f.CreateMessages(5)
	for _, id := range ids {
		r.NoError(f.Store.IndexStoredMessageContext(t.Context(), id))
	}
	first, err := f.Store.DiagnoseFTSContext(t.Context(), store.FTSDiagnosticOptions{Limit: 2})
	r.NoError(err)
	a.Equal("incomplete", first.Outcome)
	a.False(first.Complete)
	a.Equal(ids[4], first.ThroughID)
	r.NotEmpty(first.NextCursor)
	// New IDs cannot extend an in-progress interval.
	newID := f.CreateMessage("later")
	r.NoError(f.Store.IndexStoredMessageContext(t.Context(), newID))
	retry, err := f.Store.DiagnoseFTSContext(t.Context(), store.FTSDiagnosticOptions{Limit: 2})
	r.NoError(err)
	a.Equal(first.Entries, retry.Entries)
	seen := []int64{first.Entries[0].MessageID, first.Entries[1].MessageID}
	cursor := first.NextCursor
	for cursor != "" {
		page, err := f.Store.DiagnoseFTSContext(t.Context(), store.FTSDiagnosticOptions{Limit: 2, Cursor: cursor})
		r.NoError(err)
		a.Equal(first.ThroughID, page.ThroughID)
		for _, entry := range page.Entries {
			seen = append(seen, entry.MessageID)
		}
		cursor = page.NextCursor
		if cursor == "" {
			a.True(page.Complete)
			a.Equal("ok", page.Outcome)
		}
	}
	a.Equal(ids, seen)
	other := storetest.New(t)
	_, err = other.Store.DiagnoseFTSContext(t.Context(), store.FTSDiagnosticOptions{Cursor: first.NextCursor})
	r.ErrorContains(err, "invalid FTS diagnostic cursor")
}

func TestFTSDiagnosticInputValidationAndCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	for _, opts := range []store.FTSDiagnosticOptions{{Limit: -1}, {Limit: 1001}, {Timeout: 6 * time.Minute}, {Cursor: "invalid"}} {
		_, err := f.Store.DiagnoseFTSContext(t.Context(), opts)
		require.Error(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := f.Store.DiagnoseFTSContext(ctx, store.FTSDiagnosticOptions{})
	require.Error(err)
	assert.False(got.Complete)
	assert.Equal("incomplete", got.Outcome)
}

func TestFTSDiagnosticRecipientLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("many-recipients")
	participants := make([]int64, 257)
	for i := range participants {
		participants[i] = f.EnsureParticipant(fmt.Sprintf("recipient-%d@example.test", i), "Synthetic", "example.test")
	}
	require.NoError(f.Store.ReplaceMessageRecipientsContext(t.Context(), id, "to", participants, nil))
	require.NoError(f.Store.IndexStoredMessageContext(t.Context(), id))
	got, err := f.Store.DiagnoseFTSContext(t.Context(), store.FTSDiagnosticOptions{})
	require.NoError(err)
	require.Len(got.Entries, 1)
	assert.Equal("incomplete", got.Outcome)
	assert.Equal("input_limit", got.Entries[0].Reason)
}

func TestFTSDiagnosticSQLiteIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		t.Skip("SQLite FTS5 row identity")
	}
	id := f.CreateMessage("synthetic")
	require.NoError(f.Store.IndexStoredMessageContext(t.Context(), id))
	_, err := f.Store.DB().ExecContext(t.Context(), `UPDATE messages_fts SET message_id = NULL WHERE rowid = ?`, id)
	require.NoError(err)
	got, err := f.Store.DiagnoseFTSContext(t.Context(), store.FTSDiagnosticOptions{})
	require.NoError(err)
	require.Len(got.Entries, 1)
	assert.Equal("defects", got.Outcome)
	assert.Equal([]string{"identity_mismatch"}, got.Entries[0].Issues)
}

func TestPGFTSDiagnosticVersionAuxiliaryAndOverflow(t *testing.T) {
	skipUnlessPostgres(t)
	for _, tc := range []struct{ name, body, sql, issue, reason string }{
		{name: "old version", body: "orchard", sql: `indexing_version = 2`, issue: "obsolete_version"},
		{name: "missing auxiliary", body: strings.Repeat("orchard ", 260), sql: `search_fts_fields = NULL`, issue: "auxiliary_mismatch"},
		{name: "wrong auxiliary", body: strings.Repeat("orchard ", 260), sql: `search_fts_fields = ARRAY['unrelated']`, issue: "auxiliary_mismatch"},
		{name: "unnecessary auxiliary", body: "orchard", sql: `search_fts_fields = ARRAY['unrelated']`, issue: "auxiliary_mismatch"},
		{name: "unexplained empty vector", body: "", sql: `search_fts = ''::tsvector`, issue: "document_mismatch"},
		{name: "overflow terminal", body: denseShortTokenBody(), reason: "expected_vector_overflow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, r := assert.New(t), require.New(t)
			f := storetest.New(t)
			id, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
				Message:  &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "synthetic", MessageType: "email"},
				BodyText: sql.NullString{String: tc.body, Valid: true}, FTS: &store.FTSDoc{Body: tc.body},
			})
			r.NoError(err)
			if tc.sql != "" {
				_, err := f.Store.DB().ExecContext(t.Context(), `UPDATE messages SET `+tc.sql+` WHERE id = $1`, id)
				r.NoError(err)
			}
			got, err := f.Store.DiagnoseFTSContext(t.Context(), store.FTSDiagnosticOptions{})
			r.NoError(err)
			r.Len(got.Entries, 1)
			needs, err := f.Store.NeedsFTSBackfillContext(t.Context())
			r.NoError(err)
			a.Equal(tc.issue == "obsolete_version", needs, "readiness does not compare current vectors or auxiliary fields")
			if tc.reason != "" {
				a.Equal("incomplete", got.Outcome)
				a.Equal(tc.reason, got.Entries[0].Reason)
				a.Equal(1, got.Inconclusive)
			} else {
				a.Equal("defects", got.Outcome)
				a.Contains(got.Entries[0].Issues, tc.issue)
			}
		})
	}
}

func TestFTSDiagnosticSQLiteEmbeddedNUL(t *testing.T) {
	for _, field := range []string{"body", "subject", "from", "to", "cc"} {
		for _, tc := range []struct {
			name, value, indexed, outcome, reason string
		}{
			{name: "matching", value: "alpha\x00omega", indexed: "alpha\x00omega", outcome: "ok"},
			{name: "leading NUL", value: "\x00omega", indexed: "\x00omega", outcome: "ok"},
			{name: "truncated", value: "alpha\x00omega", indexed: "alpha", outcome: "defects"},
			{name: "UTF-8 below byte limit", value: strings.Repeat("α", 499_980) + "\x00x", indexed: strings.Repeat("α", 499_980) + "\x00x", outcome: "ok"},
			{name: "byte limit after NUL", value: "alpha\x00" + strings.Repeat("α", 500_001), indexed: "alpha", outcome: "incomplete", reason: "input_limit"},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				a, r := assert.New(t), require.New(t)
				f := storetest.New(t)
				if f.Store.IsPostgreSQL() {
					t.Skip("PostgreSQL TEXT cannot contain embedded NUL")
				}
				id := f.CreateMessage("synthetic-nul")
				indexed := map[string]string{field: tc.indexed}
				switch field {
				case "body":
					r.NoError(f.Store.UpsertMessageBodyContext(t.Context(), id, sql.NullString{String: tc.value, Valid: true}, sql.NullString{}))
				case "subject":
					_, err := f.Store.DB().ExecContext(t.Context(), `UPDATE messages SET subject = ? WHERE id = ?`, tc.value, id)
					r.NoError(err)
				default:
					participant := f.EnsureParticipant("synthetic@example.test", "Synthetic", "example.test")
					r.NoError(f.Store.ReplaceMessageRecipientsContext(t.Context(), id, field, []int64{participant}, nil))
					// Model historical retained text without changing an indexing writer.
					_, err := f.Store.DB().ExecContext(t.Context(), `UPDATE message_recipients SET email_address = ? WHERE message_id = ?`, tc.value, id)
					r.NoError(err)
				}
				r.NoError(f.Store.UpsertFTSContext(t.Context(), id, indexed["subject"], indexed["body"], indexed["from"], indexed["to"], indexed["cc"]))
				got, err := f.Store.DiagnoseFTSContext(t.Context(), store.FTSDiagnosticOptions{})
				r.NoError(err)
				r.Len(got.Entries, 1)
				a.Equal(tc.outcome, got.Outcome)
				a.Equal(tc.reason, got.Entries[0].Reason)
				switch tc.outcome {
				case "ok":
					a.Equal(1, got.Matched)
					a.False(got.Entries[0].EmptyDocument)
				case "defects":
					a.Equal([]string{"document_mismatch"}, got.Entries[0].Issues)
				case "incomplete":
					a.Equal(1, got.Inconclusive)
					a.Zero(got.Matched)
				}
			})
		}
	}
}

func TestFTSDiagnosticSQLiteRecipientNULPreflight(t *testing.T) {
	a, r := assert.New(t), require.New(t)
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL TEXT cannot contain embedded NUL")
	}
	id := f.CreateMessage("synthetic-recipient-nul")
	participant := f.EnsureParticipant("synthetic@example.test", "Synthetic", "example.test")
	// Bcc is not part of the compared search document. Only the recipient
	// precheck can catch this oversized value.
	r.NoError(f.Store.ReplaceMessageRecipientsContext(t.Context(), id, "bcc", []int64{participant}, nil))
	_, err := f.Store.DB().ExecContext(t.Context(), `UPDATE message_recipients SET display_name = ? WHERE message_id = ?`, "prefix\x00"+strings.Repeat("x", 1_000_001), id)
	r.NoError(err)
	r.NoError(f.Store.IndexStoredMessageContext(t.Context(), id))
	got, err := f.Store.DiagnoseFTSContext(t.Context(), store.FTSDiagnosticOptions{})
	r.NoError(err)
	r.Len(got.Entries, 1)
	a.Equal("incomplete", got.Outcome)
	a.Equal("input_limit", got.Entries[0].Reason)
	a.Equal(1, got.Inconclusive)
}
