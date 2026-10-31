package matrix

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix/crypto/olm"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type recoveryFixture struct {
	backup     *backupFixture
	store      *store.Store
	sourceID   int64
	server     *httptest.Server
	timeline   []*event.Event
	edits      []*event.Event
	quiet      bool
	extraRooms map[string][]*event.Event
	nextBatch  string
}

type ingestDuringDecryption struct {
	olm.InboundGroupSession

	ingest func() error
}

func (s ingestDuringDecryption) Decrypt(message []byte) ([]byte, uint, error) {
	if err := s.ingest(); err != nil {
		return nil, 0, err
	}
	plain, index, err := s.InboundGroupSession.Decrypt(message)
	if err != nil {
		return plain, index, fmt.Errorf("decrypt recovery fixture: %w", err)
	}
	return plain, index, nil
}

func TestImportRecoveryLockTimingAndStaleRowRejection(t *testing.T) {
	for _, mode := range []string{"ingest during decrypt", "empty recovery page avoids write lock", "reused", "deletion changed"} {
		t.Run(mode, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			original := f.message(t, "$original", "original", 10)
			if mode == "empty recovery page avoids write lock" {
				original.Content.AsEncrypted().SessionID = "absent"
			}
			f.timeline = []*event.Event{original}
			_, err := f.run(t, false, false)
			req.NoError(err)
			if mode == "reused" {
				req.NoError(f.store.MarkMessageDeleted(f.sourceID, "$original"))
			}
			rooms, err := f.store.MatrixPlaceholderRooms(t.Context(), f.sourceID, nil, nil)
			req.NoError(err)
			req.Len(rooms, 1)
			r := backupRuntime(t, f.server.URL, f.backup)
			sum := &ImportSummary{PendingEncrypted: 1}
			if mode == "empty recovery page avoids write lock" {
				// An empty recovery page must finish while another transaction holds the write lock.
				req.NoError(f.store.MatrixRecoveryTransaction(t.Context(), func(_ *store.Store) error {
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					retired, err := NewImporter(f.store, r).recoverRoom(ctx, f.sourceID, rooms[0], 0, sum)
					check.Zero(retired)
					return err
				}))
				check.Zero(sum.Recovered)
				return
			}
			var otherRoom int64
			if mode == "ingest during decrypt" {
				otherRoom, err = f.store.EnsureConversation(f.sourceID, "!other:example.org", "Other")
				req.NoError(err)
			}
			ingest := func() error {
				if mode == "ingest during decrypt" {
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					_, err = f.store.PersistMessageContext(ctx, &store.MessagePersistData{Message: &store.Message{SourceID: f.sourceID, ConversationID: otherRoom, SourceMessageID: "$other-ingest", MessageType: SourceType}})
					return err
				}
				if mode == "deletion changed" {
					return f.store.MarkMessageDeleted(f.sourceID, "$original")
				}
				if f.store.IsPostgreSQL() {
					// GC is SQLite-only; exercise the equivalent hard delete on PostgreSQL.
					_, err = f.store.DB().Exec(`DELETE FROM messages WHERE id = $1`, rooms[0].MaxMessageID)
					req.NoError(err)
				} else {
					plan, err := f.store.PlanGCContext(t.Context())
					req.NoError(err)
					deleted, err := f.store.ExecuteGCContext(t.Context(), plan)
					req.NoError(err)
					req.Equal(int64(1), deleted)
				}
				override := ""
				if f.store.IsPostgreSQL() {
					override = " OVERRIDING SYSTEM VALUE"
				}
				_, err = f.store.DB().Exec(f.store.Rebind(`INSERT INTO messages (id, source_id, conversation_id, source_message_id, message_type, snippet, deleted_from_source_at, deleted_at)`+override+` VALUES (?, ?, ?, '$replacement', 'matrix', ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`), rooms[0].MaxMessageID, f.sourceID, rooms[0].ConversationID, encryptedPlaceholder)
				return err
			}
			importSession := olm.InitInboundGroupSessionImport
			t.Cleanup(func() { olm.InitInboundGroupSessionImport = importSession })
			olm.InitInboundGroupSessionImport = func(key []byte) (olm.InboundGroupSession, error) {
				session, err := importSession(key)
				if err != nil {
					return nil, err
				}
				return ingestDuringDecryption{session, ingest}, nil
			}
			retired, err := NewImporter(f.store, r).recoverRoom(t.Context(), f.sourceID, rooms[0], 0, sum)
			req.NoError(err)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$original", "$replacement", "$other-ingest"})
			req.NoError(err)
			if mode == "ingest during decrypt" {
				check.Equal(int64(1), retired)
				check.NotZero(ids["$other-ingest"])
				check.Equal(int64(1), sum.Recovered)
				check.Zero(sum.PendingEncrypted)
				check.Equal("original", f.body(t, "$original"))
			} else {
				check.Zero(retired)
				check.Zero(sum.Recovered)
				if mode == "deletion changed" {
					check.Equal(encryptedPlaceholder, f.body(t, "$original"))
				} else {
					check.Zero(ids["$original"])
					check.Equal(rooms[0].MaxMessageID, ids["$replacement"])
				}
			}
		})
	}
}

func TestImportRecoveryContinuesAfterTransientFailureAndStopsAfterAuthFailure(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusUnauthorized} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			failed := f.message(t, "$failed", "failed", 10)
			failed.RoomID = "!first:example.org"
			f.extraRooms = map[string][]*event.Event{"!first:example.org": {failed}}
			f.timeline = []*event.Event{f.message(t, "$healthy", "healthy", 20)}
			_, err := f.run(t, false, false)
			req.NoError(err)
			f.quiet = true
			f.backup.roomStatuses = map[string]int{"!first:example.org": status}
			sum, err := f.run(t, true, false)
			if status == http.StatusServiceUnavailable {
				req.ErrorContains(err, "recover Matrix room !first:example.org")
				req.NoError(sum.BackupFailure)
				check.Equal(int64(1), sum.Recovered)
				check.Equal("healthy", f.body(t, "$healthy"))
				check.Equal(2, f.backup.roomFetches)
			} else {
				req.NoError(err)
				req.Error(sum.BackupFailure)
				check.Zero(sum.Recovered)
				check.Equal(encryptedPlaceholder, f.body(t, "$healthy"))
				check.Equal(1, f.backup.roomFetches)
			}
			check.Equal(encryptedPlaceholder, f.body(t, "$failed"))
		})
	}
}

func TestImportRecoveryCorruptPendingEdits(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{f.message(t, "$poison", "poison", 10)}
	_, err := f.run(t, false, false)
	req.NoError(err)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$poison"})
	req.NoError(err)
	req.NoError(f.store.SetMessageMetadataContext(t.Context(), ids["$poison"], sql.NullString{String: `{"matrix_event_type":"m.room.encrypted","matrix_pending_edits":"corrupt"}`, Valid: true}))
	f.quiet = true
	f.nextBatch = "after"
	_, err = f.run(t, true, false)
	req.ErrorContains(err, "recover Matrix room !room:example.org")
	run := f.operation(t)
	check.Equal(operations.StatePartial, run.State)
	check.Contains(run.Counters, operations.PublicCounter{Name: operations.CounterItemErrors, Unit: operations.CounterUnitMessages, Value: 1})
	state := f.syncState(t)
	check.Equal(f.nextBatch, state.NextBatch)
}

func TestImportEditRedactionHandlesUnavailableAndPendingEdits(t *testing.T) {
	for _, mode := range []string{"missing session", "transient"} {
		t.Run(mode, func(t *testing.T) {
			req := require.New(t)
			check := assert.New(t)
			f := newRecoveryFixture(t)
			original := f.message(t, "$original", "original", 10)
			older := f.edit(t, "$older", "$original", "older", 20)
			newest := f.edit(t, "$newest", "$original", "newest", 30)
			f.timeline = []*event.Event{original, older, newest}
			_, err := f.run(t, true, false)
			req.NoError(err)
			check.Equal("newest", f.body(t, "$original"))
			unavailable := f.edit(t, "$unavailable", "$original", "unavailable", 25)
			unavailable.Content.AsEncrypted().SessionID = "absent"
			f.edits = []*event.Event{unavailable, older, newest}
			f.timeline = []*event.Event{{ID: "$redaction", Type: event.EventRedaction, Redacts: "$newest", Content: event.Content{Raw: map[string]any{}}}}
			if mode == "transient" {
				f.backup.status = http.StatusServiceUnavailable
			}
			_, err = f.run(t, true, false)
			if mode == "transient" {
				req.ErrorContains(err, "fetch Matrix room backup")
			} else {
				req.NoError(err)
			}
			want := "original"
			if mode == "missing session" {
				want = "older"
			}
			check.Equal(want, f.body(t, "$original"))
			if mode == "transient" {
				f.quiet, f.backup.status, f.backup.etag = true, 0, "2"
				_, err = f.run(t, true, false)
				req.NoError(err)
				check.Equal("older", f.body(t, "$original"))
			}
		})
	}
	t.Run("redacted encrypted", func(t *testing.T) {
		req, check := require.New(t), assert.New(t)
		f := newRecoveryFixture(t)
		original := f.message(t, "$z-original", "original", 10)
		initialKey := f.backup.data.SessionKey
		{
			in, err := olm.NewInboundGroupSession([]byte(f.backup.out.Key()))
			req.NoError(err)
			exported, err := in.Export(1)
			req.NoError(err)
			f.backup.data.SessionKey = string(exported)
		}
		edit := f.edit(t, "$edit", "$z-original", "updated", 20)
		f.timeline = []*event.Event{original, edit}
		_, err := f.run(t, true, false)
		req.NoError(err)
		check.Equal(encryptedPlaceholder, f.body(t, "$z-original"))
		f.quiet = true
		sum, err := f.run(t, true, true)
		req.NoError(err)
		check.Equal(int64(1), sum.PendingEncrypted)
		f.quiet = false
		f.timeline = []*event.Event{{ID: "$redaction", Type: event.EventRedaction, Redacts: "$edit", Content: event.Content{Raw: map[string]any{}}}}
		_, err = f.run(t, true, false)
		req.NoError(err)
		f.quiet, f.backup.data.SessionKey = true, initialKey
		_, err = f.run(t, true, true)
		req.NoError(err)
		check.Equal("original", f.body(t, "$z-original"))
	})
}

func TestImportRecoverySkipsBackupWhenDisabledOrRoomExcluded(t *testing.T) {
	for _, mode := range []string{"SkipKeyBackup", "ExcludeRooms"} {
		t.Run(mode, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			f.timeline = []*event.Event{f.message(t, "$pending", "pending", 40)}
			opts := ImportOptions{UserID: "@archive:example.org"}
			if mode == "SkipKeyBackup" {
				opts.SkipKeyBackup = true
			} else {
				_, err := f.run(t, false, false)
				req.NoError(err)
				f.quiet, f.backup.versionStatus = true, http.StatusServiceUnavailable
				opts.ExcludeRooms = []string{"!room:example.org"}
			}
			versions, rooms := f.backup.versionFetches, f.backup.roomFetches
			sum, err := NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(t.Context(), opts)
			req.NoError(err)
			check.Equal(versions, f.backup.versionFetches)
			check.Equal(rooms, f.backup.roomFetches)
			req.NoError(sum.BackupFailure)
			if mode == "SkipKeyBackup" {
				check.Equal(encryptedPlaceholder, f.body(t, "$pending"))
				check.Zero(sum.Recovered)
			}
		})
	}
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	req := require.New(t)
	f := &recoveryFixture{backup: newBackupFixture(t), store: testutil.NewTestStore(t), nextBatch: "next"}
	source, err := f.store.GetOrCreateSource(SourceType, "@archive:example.org")
	req.NoError(err)
	f.sourceID = source.ID
	mux := http.NewServeMux()
	f.backup.handle(t, mux)
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		join := map[string]any{}
		if !f.quiet {
			for room, events := range f.extraRooms {
				join[room] = map[string]any{"timeline": map[string]any{"events": events}}
			}
			join["!room:example.org"] = map[string]any{"timeline": map[string]any{"events": f.timeline, "prev_batch": "history"}}
		}
		rooms := map[string]any{"join": join}
		writeMatrixJSON(t, w, map[string]any{"next_batch": f.nextBatch, "rooms": rooms})
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/{user}/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"@member:example.org":["!room:example.org"]}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/{room}/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@member:example.org":{}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/{room}/messages", func(w http.ResponseWriter, _ *http.Request) {
		writeMatrixJSON(t, w, map[string]any{"chunk": []*event.Event{}, "end": ""})
	})
	mux.HandleFunc("GET /_matrix/client/v1/rooms/{room}/relations/{event}/m.replace", func(w http.ResponseWriter, _ *http.Request) { writeMatrixJSON(t, w, map[string]any{"chunk": f.edits}) })
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *recoveryFixture) run(t *testing.T, key, full bool) (*ImportSummary, error) {
	t.Helper()
	r := backupRuntime(t, f.server.URL, f.backup)
	if !key {
		r.key = nil
	}
	return NewImporter(f.store, r).Import(t.Context(), ImportOptions{UserID: "@archive:example.org", Full: full})
}

func (f *recoveryFixture) message(t *testing.T, eventID, body string, ts int64) *event.Event {
	t.Helper()
	return f.backup.encrypted(t, eventID, ts, event.EventMessage, map[string]any{"msgtype": "m.text", "body": body})
}

func (f *recoveryFixture) edit(t *testing.T, eventID, target, body string, ts int64) *event.Event {
	t.Helper()
	return f.backup.encrypted(t, eventID, ts, event.EventMessage, map[string]any{"msgtype": "m.text", "body": "* " + body, "m.new_content": map[string]any{"msgtype": "m.text", "body": body}, "m.relates_to": map[string]any{"rel_type": "m.replace", "event_id": target}})
}

func TestImportFullSyncKeepsRecoveredReactionRedacted(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	original := f.message(t, "$original", "original", 10)
	reaction := f.backup.encrypted(t, "$reaction", 20, event.EventReaction, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$original", "key": "yes"}})
	f.timeline = []*event.Event{original, reaction}
	_, err := f.run(t, true, false)
	req.NoError(err)
	var reactions int
	req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&reactions))
	check.Equal(1, reactions)
	f.timeline = append(f.timeline, &event.Event{ID: "$redaction", Type: event.EventRedaction, Redacts: "$reaction", Content: event.Content{Raw: map[string]any{}}})
	_, err = f.run(t, false, true)
	req.NoError(err)
	req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&reactions))
	check.Zero(reactions)
	f.quiet = true
	_, err = f.run(t, true, false)
	req.NoError(err)
	req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&reactions))
	check.Zero(reactions)
}

func TestImportRecoversMessagesAndEdits(t *testing.T) {
	t.Run("messages and edits", func(t *testing.T) {
		req := require.New(t)
		check := assert.New(t)
		f := newRecoveryFixture(t)
		oldEdit := f.edit(t, "$a-edit", "$z-original", "older", 20)
		newEdit := f.edit(t, "$b-edit", "$z-original", "newest", 30)
		reply := f.backup.encrypted(t, "$reply", 40, event.EventMessage, map[string]any{"msgtype": "m.text", "body": "reply", "m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": "$z-original"}}})
		f.timeline = []*event.Event{oldEdit, newEdit, reply, f.message(t, "$z-original", "original", 10)}
		_, err := f.run(t, false, false)
		req.NoError(err)
		check.Equal(encryptedPlaceholder, f.body(t, "$z-original"))
		f.quiet = true
		sum, err := f.run(t, true, false)
		req.NoError(err)
		check.Equal(int64(2), sum.Recovered)
		check.Equal(int64(2), sum.MessagesProcessed)
		run := f.operation(t)
		check.Equal(operations.StateSucceeded, run.State)
		check.Contains(run.Counters, operations.PublicCounter{Name: operations.CounterUpdated, Unit: operations.CounterUnitMessages, Value: 2})
		check.Zero(sum.PendingEncrypted)
		check.Equal("newest", f.body(t, "$z-original"))
	})
	t.Run("crosses placeholder pages", func(t *testing.T) {
		req := require.New(t)
		check := assert.New(t)
		f := newRecoveryFixture(t)
		f.timeline = []*event.Event{f.edit(t, "$000-edit", "$zzz-original", "updated", 20)}
		for i := range 500 {
			f.timeline = append(f.timeline, f.message(t, fmt.Sprintf("$%03d", i), "filler", 10))
		}
		f.timeline = append(f.timeline, f.message(t, "$zzz-original", "original", 10))
		_, err := f.run(t, false, false)
		req.NoError(err)
		f.quiet = true
		_, err = f.run(t, true, false)
		req.NoError(err)
		check.Equal("updated", f.body(t, "$zzz-original"))
	})
	t.Run("plaintext edit", func(t *testing.T) {
		req, check := require.New(t), assert.New(t)
		f := newRecoveryFixture(t)
		original := f.message(t, "$z-original", "original", 10)
		edit := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":20,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$z-original"}}}`)
		f.timeline = []*event.Event{original, edit}
		_, err := f.run(t, false, false)
		req.NoError(err)
		f.quiet = true
		_, err = f.run(t, true, false)
		req.NoError(err)
		ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$z-original", "$edit"})
		req.NoError(err)
		check.Zero(ids["$edit"], "edits do not create message rows")
		check.Equal("edited", f.body(t, "$z-original"))
		metadata, err := f.store.GetMessageMetadata(ids["$z-original"])
		req.NoError(err)
		check.NotContains(metadata.String, "matrix_pending_edits")
	})
}

func (f *recoveryFixture) body(t *testing.T, eventID string) string {
	t.Helper()
	found, err := f.store.MessageExistsBatch(f.sourceID, []string{eventID})
	require.NoError(t, err)
	require.NotZero(t, found[eventID])
	body, err := f.store.GetMessageBodyText(found[eventID])
	require.NoError(t, err)
	return body
}

func (f *recoveryFixture) operation(t *testing.T) operations.Run {
	t.Helper()
	req := require.New(t)
	last, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
	req.NoError(err)
	id, err := operations.NewInt64ID(operations.KindSourceSync, last.ID)
	req.NoError(err)
	run, err := f.store.GetRun(t.Context(), id)
	req.NoError(err)
	return run
}

func (f *recoveryFixture) syncState(t *testing.T) *SyncState {
	t.Helper()
	last, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
	require.NoError(t, err)
	state, err := loadSyncState(last.CursorAfter.String)
	require.NoError(t, err)
	return state
}

func (f *recoveryFixture) ageAttempts(t *testing.T) {
	t.Helper()
	req := require.New(t)
	state := f.syncState(t)
	for room, attempt := range state.RecoveryRooms {
		attempt.TriedAt = time.Now().Add(-2 * recoveryCooldown)
		attempt.NoProgressAt = attempt.TriedAt
		state.RecoveryRooms[room] = attempt
	}
	encoded, err := state.marshal()
	req.NoError(err)
	syncID, err := f.store.StartSync(f.sourceID, SourceType)
	req.NoError(err)
	req.NoError(f.store.CompleteSyncAndUpdateSourceCursor(syncID, f.sourceID, encoded))
}

func TestImportRecoveryDeletedPlaceholders(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{f.backup.encrypted(t, "$original", 20, event.EventMessage, map[string]any{"msgtype": "m.text", "body": "original", "m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": "$target"}}})}
	_, err := f.run(t, false, false)
	req.NoError(err)
	f.timeline = []*event.Event{{ID: "$redaction", Type: event.EventRedaction, Redacts: "$original", Content: event.Content{Raw: map[string]any{}}}}
	_, err = f.run(t, false, false)
	req.NoError(err)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$original"})
	req.NoError(err)
	req.NotZero(ids["$original"])
	f.quiet = true
	sum, err := f.run(t, true, false)
	req.NoError(err)
	check.Zero(sum.PendingEncrypted)
	check.Equal("original", f.body(t, "$original"))
	var deleted bool
	req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), ids["$original"]).Scan(&deleted))
	check.True(deleted)
	f.quiet = false
	f.timeline = []*event.Event{f.message(t, "$target", "target", 10)}
	_, err = f.run(t, true, false)
	req.NoError(err)
	ids, err = f.store.MessageExistsBatch(f.sourceID, []string{"$original", "$target"})
	req.NoError(err)
	var targetID int64
	req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), ids["$original"]).Scan(&targetID))
	check.Equal(ids["$target"], targetID)
}

func TestImportRecoveryRollsBackInterruptedWrites(t *testing.T) {
	for _, mode := range []string{"between originals", "reaction/resumed"} {
		t.Run(mode, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			original := f.backup.encrypted(t, "$original", 10, event.EventMessage, map[string]any{"msgtype": "m.text", "body": "original", "m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": "$later"}}})
			f.timeline = []*event.Event{original}
			if mode == "reaction/resumed" {
				_, err := f.run(t, true, false)
				req.NoError(err)
				f.timeline = nil
			}
			f.timeline = append(f.timeline, f.backup.encrypted(t, "$reaction", 20, event.EventReaction, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$original", "key": "yes"}}))
			operation, record, stopID := "UPDATE", "NEW", "$second"
			if mode == "between originals" {
				f.timeline = append(f.timeline, f.message(t, "$second", "second", 30))
			} else {
				f.timeline = append(f.timeline, f.backup.encrypted(t, "$second-reaction", 30, event.EventReaction, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$original", "key": "second"}}))
				operation, record, stopID = "DELETE", "OLD", "$second-reaction"
			}
			_, err := f.run(t, false, false)
			req.NoError(err)
			create := fmt.Sprintf(`CREATE TRIGGER stop_recovery BEFORE %s ON messages WHEN %s.source_message_id = '%s' BEGIN SELECT RAISE(ABORT, 'interrupted recovery'); END`, operation, record, stopID)
			drop := `DROP TRIGGER stop_recovery`
			if f.store.IsPostgreSQL() {
				_, err := f.store.DB().Exec(fmt.Sprintf(`CREATE FUNCTION stop_recovery_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF %s.source_message_id = '%s' THEN RAISE EXCEPTION 'interrupted recovery'; END IF; RETURN %s; END $$`, record, stopID, record))
				req.NoError(err)
				create = fmt.Sprintf(`CREATE TRIGGER stop_recovery BEFORE %s ON messages FOR EACH ROW EXECUTE FUNCTION stop_recovery_fn()`, operation)
				drop = `DROP TRIGGER stop_recovery ON messages`
			}
			_, err = f.store.DB().Exec(create)
			req.NoError(err)
			f.quiet = true
			_, err = f.run(t, true, false)
			req.ErrorContains(err, "interrupted recovery")
			wantBody, wantReactions := "original", 2
			if mode == "between originals" {
				state := f.syncState(t)
				check.Empty(state.Rooms["!room:example.org"].DeferredRelations, "rolled-back replies must stay out of the cursor")
				check.Equal(encryptedPlaceholder, f.body(t, "$original"))
				wantReactions = 1
			} else {
				check.Equal(wantBody, f.body(t, "$original"))
			}
			check.Equal(encryptedPlaceholder, f.body(t, stopID))
			check.Equal(encryptedPlaceholder, f.body(t, "$reaction"))
			var reactions int
			req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&reactions))
			check.Zero(reactions)
			_, err = f.store.DB().Exec(drop)
			req.NoError(err)
			sum, err := f.run(t, true, false)
			req.NoError(err)
			check.Zero(sum.PendingEncrypted)
			check.Equal(wantBody, f.body(t, "$original"))
			req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&reactions))
			check.Equal(wantReactions, reactions)
			if mode == "between originals" {
				check.Equal("second", f.body(t, "$second"))
				sum, err := f.run(t, true, false)
				req.NoError(err)
				check.Zero(sum.Recovered)
				check.Zero(sum.PendingEncrypted)
				req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&reactions))
				check.Equal(1, reactions)
				ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$original", "$second", "$reaction"})
				req.NoError(err)
				check.Len(ids, 2)
			}
		})
	}
}

func TestImportRecoveryResumesBeforePlaceholderCheckpoint(t *testing.T) {
	for _, mode := range []string{"placeholder arrives in same sync", "quiet follow-up sync"} {
		t.Run(mode, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			missing := f.message(t, "$missing", "missing", 10)
			missing.Content.AsEncrypted().SessionID = "absent"
			f.timeline = []*event.Event{missing}
			_, err := f.run(t, true, false)
			req.NoError(err)
			convID, err := f.store.EnsureConversation(f.sourceID, "!room:example.org", "")
			req.NoError(err)
			fresh := f.message(t, "$fresh", "fresh", 20)
			fresh.RoomID = "!room:example.org"
			// Commit the placeholder, then discard the importer before it checkpoints.
			req.NoError(NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).persistEvent(t.Context(), f.sourceID, convID, fresh, &ImportSummary{}))
			f.timeline, f.quiet = []*event.Event{fresh}, mode == "quiet follow-up sync"
			sum, err := f.run(t, true, false)
			req.NoError(err)
			check.Equal(int64(1), sum.Recovered)
			check.Equal(int64(1), sum.MessagesProcessed)
			check.Equal(int64(1), sum.PendingEncrypted)
			check.Equal("fresh", f.body(t, "$fresh"))
			check.Equal(2, f.backup.roomFetches)
			f.quiet = true
			_, err = f.run(t, true, false)
			req.NoError(err)
			check.Equal(2, f.backup.roomFetches)
		})
	}
}

func TestImportRecoveryBoundsRetriesAndAttemptsNewEvents(t *testing.T) {
	t.Run("bounded retries", func(t *testing.T) {
		for _, mode := range []string{"missing sessions", "failed room fetches"} {
			t.Run(mode, func(t *testing.T) {
				req := require.New(t)
				check := assert.New(t)
				f := newRecoveryFixture(t)
				f.extraRooms = map[string][]*event.Event{}
				f.backup.roomStatuses = map[string]int{}
				const roomCount = 3 * recoveryRetryLimit
				for i := range roomCount {
					evt := f.message(t, "$missing"+strconv.Itoa(i), "missing", 10)
					roomID := fmt.Sprintf("!missing%d:example.org", i)
					evt.RoomID = id.RoomID(roomID)
					if mode == "missing sessions" {
						evt.Content.AsEncrypted().SessionID = "absent"
					} else {
						f.backup.roomStatuses[roomID] = http.StatusServiceUnavailable
					}
					f.extraRooms[roomID] = []*event.Event{evt}
				}
				if mode == "failed room fetches" {
					_, err := f.run(t, false, false)
					req.NoError(err)
					f.quiet = true
					for round := range 3 {
						sum, err := f.run(t, true, false)
						req.ErrorContains(err, "recover Matrix room")
						check.Equal(int64(roomCount), sum.PendingEncrypted)
						check.Equal(roomCount+round*recoveryRetryLimit, f.backup.roomFetches)
					}
					return
				}
				_, err := f.run(t, true, false)
				req.NoError(err)
				check.Equal(roomCount, f.backup.roomFetches)
				f.quiet = true
				_, err = f.run(t, true, false)
				req.NoError(err)
				check.Equal(roomCount, f.backup.roomFetches)
				f.backup.etag = "2"
				_, err = f.run(t, true, false)
				req.NoError(err)
				check.Equal(roomCount, f.backup.roomFetches, "cooldown holds even when the backup changes")
				f.ageAttempts(t)
				_, err = f.run(t, true, false)
				req.NoError(err)
				check.Equal(roomCount+recoveryRetryLimit, f.backup.roomFetches)
				_, err = f.run(t, true, false)
				req.NoError(err)
				check.Equal(roomCount+2*recoveryRetryLimit, f.backup.roomFetches)
				f.quiet = false
				f.timeline = []*event.Event{f.message(t, "$new", "new", 20)}
				_, err = f.run(t, true, false)
				req.NoError(err)
				check.Equal("new", f.body(t, "$new"))
				check.Equal(2*roomCount+1, f.backup.roomFetches, "all old rooms have retried once, plus the new room")
				_, err = f.run(t, true, true)
				req.NoError(err)
				check.Equal(3*roomCount+1, f.backup.roomFetches, "full sync forces all pending rooms")
			})
		}
	})
	t.Run("new placeholder retries after backup version failure", func(t *testing.T) {
		req := require.New(t)
		check := assert.New(t)
		f := newRecoveryFixture(t)
		f.timeline = []*event.Event{f.message(t, "$first", "first", 10)}
		_, err := f.run(t, true, false)
		req.NoError(err)
		f.timeline = []*event.Event{f.message(t, "$new", "new", 20)}
		f.backup.versionStatus = http.StatusServiceUnavailable
		sum, err := f.run(t, true, false)
		req.NoError(err)
		req.Error(sum.BackupFailure)
		check.Equal(int64(1), sum.PendingEncrypted)
		check.Equal(encryptedPlaceholder, f.body(t, "$new"))
		f.quiet, f.backup.versionStatus = true, 0
		sum, err = f.run(t, true, false)
		req.NoError(err)
		check.Equal(int64(1), sum.Recovered)
		check.Zero(sum.PendingEncrypted)
		check.Equal("new", f.body(t, "$new"))
	})
	t.Run("new placeholder keeps old cooldown", func(t *testing.T) {
		req, check := require.New(t), assert.New(t)
		f := newRecoveryFixture(t)
		old := f.message(t, "$old", "old", 10)
		f.timeline = []*event.Event{old}
		f.backup.status = http.StatusNotFound
		_, err := f.run(t, true, false)
		req.NoError(err)
		f.backup.status, f.backup.etag = 0, "2"
		f.timeline = []*event.Event{f.message(t, "$new", "new", 20)}
		sum, err := f.run(t, true, false)
		req.NoError(err)
		check.Equal(int64(1), sum.Recovered)
		check.Equal(int64(1), sum.PendingEncrypted)
		check.Equal(encryptedPlaceholder, f.body(t, "$old"))
		check.Equal("new", f.body(t, "$new"))
		f.ageAttempts(t)
		f.quiet = true
		_, err = f.run(t, true, false)
		req.NoError(err)
		check.Equal("old", f.body(t, "$old"))
	})
	t.Run("retired relation retries after backup change", func(t *testing.T) {
		req, check := require.New(t), assert.New(t)
		f := newRecoveryFixture(t)
		missing := f.message(t, "$missing", "missing", 10)
		missing.Content.AsEncrypted().SessionID = "absent"
		deleted := f.backup.encrypted(t, "$deleted", 20, event.EventReaction, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$missing", "key": "yes"}})
		f.timeline = []*event.Event{missing, deleted}
		_, err := f.run(t, false, false)
		req.NoError(err)
		f.quiet = true
		_, err = f.run(t, true, false)
		req.NoError(err)
		ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$deleted"})
		req.NoError(err)
		req.Empty(ids)
		f.backup.etag = "2"
		sum, err := f.run(t, true, false)
		req.NoError(err)
		check.Zero(sum.Recovered)
		check.Equal(int64(1), sum.PendingEncrypted)
		check.Equal(2, f.backup.roomFetches, "retiring a relation allows an immediate retry after a backup change")
	})

	t.Run("progress does not starve quiet room", func(t *testing.T) {
		req, check := require.New(t), assert.New(t)
		f := newRecoveryFixture(t)
		messageInRoom := func(room, eventID, body string) *event.Event {
			evt := f.message(t, eventID, body, 20)
			plain, err := json.Marshal(map[string]any{"room_id": room, "type": event.EventMessage.Type, "content": map[string]any{"msgtype": "m.text", "body": body}})
			req.NoError(err)
			cipher, err := f.backup.out.Encrypt(plain)
			req.NoError(err)
			evt.Content.AsEncrypted().MegolmCiphertext = cipher
			return evt
		}
		f.extraRooms = map[string][]*event.Event{}
		f.backup.sessionResponse = map[string]any{"sessions": map[string]any{}}
		for i := range recoveryRetryLimit + 1 {
			room := fmt.Sprintf("!busy%d:example.org", i)
			missing := f.message(t, "$missing"+strconv.Itoa(i), "missing", 10)
			missing.Content.AsEncrypted().SessionID = "absent"
			if i == recoveryRetryLimit {
				missing = messageInRoom(room, "$quiet", "quiet")
			}
			f.extraRooms[room] = []*event.Event{missing}
		}
		_, err := f.run(t, true, false)
		req.NoError(err)
		f.ageAttempts(t)
		f.backup.sessionResponse = nil
		began := time.Now()
		for round := range 2 {
			for i := range recoveryRetryLimit {
				room := fmt.Sprintf("!busy%d:example.org", i)
				f.extraRooms[room] = []*event.Event{messageInRoom(room, fmt.Sprintf("$new%d-%d", round, i), "new")}
			}
			delete(f.extraRooms, fmt.Sprintf("!busy%d:example.org", recoveryRetryLimit))
			f.backup.etag = strconv.Itoa(round + 2)
			_, err := f.run(t, true, false)
			req.NoError(err)
		}
		state := f.syncState(t)
		check.False(state.RecoveryRooms[fmt.Sprintf("!busy%d:example.org", recoveryRetryLimit)].TriedAt.Before(began), "the quiet room gets a turn while busy rooms keep recovering events")
		check.Equal("quiet", f.body(t, "$quiet"), "the quiet room recovers once its keys become available")
		check.True(state.RecoveryRooms["!busy0:example.org"].NoProgressAt.IsZero(), "progress clears only the cooldown")
	})
	t.Run("interrupted new placeholders preserve recovery checkpoint", func(t *testing.T) {
		req, check := require.New(t), assert.New(t)
		f := newRecoveryFixture(t)
		f.extraRooms = map[string][]*event.Event{}
		for i := range recoveryRetryLimit + 1 {
			missing := f.message(t, fmt.Sprintf("$old%d", i), "old", 10)
			missing.Content.AsEncrypted().SessionID = "absent"
			f.extraRooms[fmt.Sprintf("!missing%d:example.org", i)] = []*event.Event{missing}
		}
		_, err := f.run(t, true, false)
		req.NoError(err)
		before := f.syncState(t)
		for i := range recoveryRetryLimit + 1 {
			fresh := f.message(t, fmt.Sprintf("$new%d", i), "new", 20)
			fresh.Content.AsEncrypted().SessionID = "absent"
			f.extraRooms[fmt.Sprintf("!missing%d:example.org", i)] = []*event.Event{fresh}
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		_, err = NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(ctx, ImportOptions{UserID: "@archive:example.org", Progress: func(line string) {
			if strings.HasPrefix(line, "!room:example.org:") {
				cancel()
			}
		}})
		req.ErrorIs(err, context.Canceled)
		checkpoint, err := f.store.GetLatestCheckpointedSyncByType(f.sourceID, SourceType)
		req.NoError(err)
		state, err := loadSyncState(checkpoint.CursorBefore.String)
		req.NoError(err)
		check.Equal(before.RecoveryRooms, state.RecoveryRooms)
	})
}

func TestImportGCRecoversReusedPlaceholderID(t *testing.T) {
	for _, mode := range []string{"same sync", "later sync", "later sync with new event"} {
		t.Run(mode, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			if f.store.IsPostgreSQL() {
				t.Skip("requires SQLite rowid reuse")
			}
			missing := f.message(t, "$missing", "missing", 10)
			missing.Content.AsEncrypted().SessionID = "absent"
			deleted := f.message(t, "$deleted", "deleted", 20)
			f.backup.status = http.StatusNotFound
			f.timeline = []*event.Event{missing, deleted}
			_, err := f.run(t, true, false)
			req.NoError(err)
			before := f.syncState(t)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$deleted"})
			req.NoError(err)
			deletedID := ids["$deleted"]
			var maxID int64
			req.NoError(f.store.DB().QueryRow(`SELECT MAX(id) FROM messages`).Scan(&maxID))
			req.Equal(maxID, deletedID)
			req.Equal(deletedID, before.RecoveryRooms["!room:example.org"].LastMessageID)
			req.NoError(f.store.MarkMessageDeleted(f.sourceID, "$deleted"))
			plan, err := f.store.PlanGCContext(t.Context())
			req.NoError(err)
			collected, err := f.store.ExecuteGCContext(t.Context(), plan)
			req.NoError(err)
			req.Equal(int64(1), collected)
			f.backup.status = 0
			f.timeline = []*event.Event{f.message(t, "$new", "new", 30)}
			if mode != "same sync" {
				_, err = f.run(t, false, false)
				req.NoError(err)
				f.quiet = true
				if mode == "later sync with new event" {
					f.quiet = false
					f.timeline = []*event.Event{missing}
					fresh := f.message(t, "$fresh", "fresh", 40)
					fresh.Content.AsEncrypted().SessionID = "absent"
					f.timeline = append(f.timeline, fresh)
				}
			}
			sum, err := f.run(t, true, false)
			req.NoError(err)
			check.Equal(int64(1), sum.Recovered)
			pending := int64(1)
			if mode == "later sync with new event" {
				pending++
			}
			check.Equal(pending, sum.PendingEncrypted)
			check.Equal("new", f.body(t, "$new"))
			ids, err = f.store.MessageExistsBatch(f.sourceID, []string{"$new"})
			req.NoError(err)
			req.Equal(deletedID, ids["$new"], "SQLite reuses the collected table-max rowid")
			after := f.syncState(t)
			check.Equal(before.RecoveryRooms["!room:example.org"].Stamp, after.RecoveryRooms["!room:example.org"].Stamp)
		})
	}
}

func TestImportRecoveryRetiresUnsupportedEventsAndKeepsMalformedPending(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{
		f.backup.encrypted(t, "$unsupported", 10, event.Type{Type: "example.unsupported"}, map[string]any{}),
		f.backup.encrypted(t, "$malformed", 20, event.EventMessage, map[string]any{"body": 42}),
	}
	_, err := f.run(t, false, false)
	req.NoError(err)
	f.quiet = true
	sum, err := f.run(t, true, false)
	req.NoError(err)
	check.Equal(int64(1), sum.PendingEncrypted)
	convID, err := f.store.EnsureConversation(f.sourceID, "!room:example.org", "")
	req.NoError(err)
	items, _, err := f.store.MatrixPlaceholders(t.Context(), convID, 0, 10)
	req.NoError(err)
	req.Len(items, 1)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$malformed"})
	req.NoError(err)
	check.Equal(ids["$malformed"], items[0].MessageID)
}

func TestMatrixCursorFormatVersion(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	for _, blob := range []string{`{"rooms":{}}`, `{"format_version":1,"rooms":{}}`} {
		state, err := loadSyncState(blob)
		req.NoError(err)
		encoded, err := state.marshal()
		req.NoError(err)
		check.Contains(encoded, `"format_version":1`)
	}
	_, err := loadSyncState(`{"format_version":2,"rooms":{}}`)
	req.ErrorContains(err, "unsupported Matrix cursor format version 2")
}

func TestImportPendingRepliesCrossPagesWithoutRawEvents(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	convID, err := f.store.EnsureConversation(f.sourceID, "!room:example.org", "Room")
	req.NoError(err)
	var replyIDs []int64
	for i := range pageSize + 1 {
		messageID, err := f.store.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: f.sourceID, ConversationID: convID, SourceMessageID: fmt.Sprintf("$reply-%d", i), MessageType: SourceType}})
		req.NoError(err)
		req.NoError(f.store.SetMessageMetadataContext(t.Context(), messageID, sql.NullString{String: `{"referenced_message_id":"$target"}`, Valid: true}))
		replyIDs = append(replyIDs, messageID)
	}
	f.timeline = []*event.Event{f.message(t, "$target", "target", 10)}
	_, err = f.run(t, false, false)
	req.NoError(err)
	targets, err := f.store.MessageExistsBatch(f.sourceID, []string{"$target"})
	req.NoError(err)
	for _, messageID := range replyIDs {
		var targetID sql.NullInt64
		req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), messageID).Scan(&targetID))
		check.Equal(targets["$target"], targetID.Int64)
	}
}

func TestImportHandlesUnreadableCursorBySyncMode(t *testing.T) {
	for _, tc := range []struct {
		name          string
		full          bool
		blob          string
		wantError     bool
		wantDecodeErr bool
		wantCursorErr bool
	}{
		{"incremental refuses malformed cursor", false, "invalid JSON", true, true, false},
		{"full replaces malformed cursor", true, "invalid JSON", false, false, false},
		{"full refuses future version", true, `{"format_version":2,"rooms":{}}`, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			syncID, err := f.store.StartSync(f.sourceID, SourceType)
			req.NoError(err)
			req.NoError(f.store.CompleteSync(syncID, tc.blob))
			handler := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.full && r.URL.Path == "/_matrix/client/v3/sync" {
					check.Empty(r.URL.Query().Get("since"))
				}
				handler.ServeHTTP(w, r)
			})
			_, err = f.run(t, true, tc.full)
			if !tc.wantError {
				req.NoError(err)
				latest, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
				req.NoError(err)
				check.NotEqual(syncID, latest.ID)
				return
			}
			req.Error(err)
			if tc.wantDecodeErr {
				req.ErrorContains(err, "decode Matrix sync state")
			}
			if tc.wantCursorErr {
				req.ErrorIs(err, errCursorVersion)
			}
			latest, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
			req.NoError(err)
			check.Equal(syncID, latest.ID)
			check.Equal(tc.blob, latest.CursorAfter.String)
		})
	}
}

func TestImportRecoveryRejectsReplayedIndex(t *testing.T) {
	for _, kind := range []string{"message", "edit", "reaction", "redacted edit", "redacted reaction", "orphan edit", "orphan reaction"} {
		retired := strings.Contains(kind, " ")
		for _, later := range []bool{false, true} {
			if retired && !later {
				continue
			}
			t.Run(kind+"/later="+strconv.FormatBool(later), func(t *testing.T) {
				req, check := require.New(t), assert.New(t)
				f := newRecoveryFixture(t)
				if !retired && kind != "message" {
					f.timeline = []*event.Event{f.message(t, "$original", "original", 1)}
					_, err := f.run(t, true, false)
					req.NoError(err)
				}
				first := f.message(t, "$first", "recovered", 10)
				if strings.HasSuffix(kind, "edit") {
					first = f.edit(t, "$first", "$original", "edited", 10)
				} else if strings.HasSuffix(kind, "reaction") {
					first = f.backup.encrypted(t, "$first", 10, event.EventReaction, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$original", "key": "yes"}})
				}
				second := *first
				second.ID, second.Sender = "$second", "@forged:example.org"
				f.timeline = []*event.Event{first}
				if retired && strings.HasPrefix(kind, "redacted") {
					f.timeline = append(f.timeline, f.message(t, "$original", "original", 1))
				}
				if !later {
					f.timeline = append(f.timeline, &second)
				}
				_, err := f.run(t, !retired, false)
				req.NoError(err)
				if retired {
					if strings.HasPrefix(kind, "redacted") {
						f.timeline = []*event.Event{{ID: "$redaction", Type: event.EventRedaction, Redacts: "$first", Content: event.Content{Raw: map[string]any{}}}}
						_, err = f.run(t, false, false)
						req.NoError(err)
					}
					f.quiet = true
					sum, err := f.run(t, true, false)
					req.NoError(err)
					check.Zero(sum.PendingEncrypted)
				}
				if later {
					f.quiet = false
					f.timeline = []*event.Event{&second}
					if strings.HasPrefix(kind, "orphan") {
						f.timeline = append(f.timeline, f.message(t, "$original", "original", 1))
					}
					_, err = f.run(t, true, false)
					req.NoError(err)
				}
				check.Equal(encryptedPlaceholder, f.body(t, "$second"))
				var recovered int
				if retired {
					check.Equal("original", f.body(t, "$original"))
					req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&recovered))
					check.Zero(recovered)
					return
				}
				ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$second"})
				req.NoError(err)
				raw, err := f.store.GetMessageRaw(ids["$second"])
				req.NoError(err)
				check.Contains(string(raw), `"ciphertext"`)
				metadata, err := f.store.GetMessageMetadata(ids["$second"])
				req.NoError(err)
				check.JSONEq(`{"matrix_event_type":"m.room.encrypted"}`, metadata.String)
				switch kind {
				case "reaction":
					req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&recovered))
					check.Equal(1, recovered)
					req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reaction_source_events WHERE source_reaction_id = '$second'`).Scan(&recovered))
					check.Zero(recovered)
				case "edit":
					check.Equal("edited", f.body(t, "$original"))
				default:
					check.Equal("recovered", f.body(t, "$first"))
				}
				f.quiet = true
				sum, err := f.run(t, true, true)
				req.NoError(err)
				check.Equal(int64(1), sum.PendingEncrypted)
				check.Zero(sum.Recovered)
				check.Equal(encryptedPlaceholder, f.body(t, "$second"))
			})
		}
	}
	for _, claimPath := range []string{"placeholder recovery", "edit rollback"} {
		t.Run(claimPath, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			original := f.message(t, "$original", "original", 10)
			other := f.message(t, "$other", "other", 11)
			older := f.edit(t, "$older", "$original", "older", 20)
			newest := f.edit(t, "$newest", "$other", "newest", 30)
			f.timeline = []*event.Event{original, other, newest}
			if claimPath == "placeholder recovery" {
				f.timeline = append(f.timeline, older)
			} else {
				f.timeline = append(f.timeline, f.edit(t, "$original-newest", "$original", "original newest", 40))
			}
			_, err := f.run(t, true, false)
			req.NoError(err)
			if claimPath == "edit rollback" {
				f.edits = []*event.Event{older}
				f.timeline = []*event.Event{{ID: "$first-redaction", Type: event.EventRedaction, Redacts: "$original-newest", Content: event.Content{Raw: map[string]any{}}}}
				_, err = f.run(t, true, false)
				req.NoError(err)
			}
			if claimPath == "placeholder recovery" {
				check.Equal("older", f.body(t, "$original"))
			}
			check.Equal("newest", f.body(t, "$other"))
			raw, err := json.Marshal(older)
			req.NoError(err)
			replayed := matrixTestEvent(t, string(raw))
			replayed.ID = "$replayed"
			req.True(parseContent(replayed))
			replayed.Content.AsEncrypted().RelatesTo.EventID = "$other"
			f.edits = []*event.Event{replayed}
			f.timeline = []*event.Event{{ID: "$redaction", Type: event.EventRedaction, Redacts: "$newest", Content: event.Content{Raw: map[string]any{}}}}
			_, err = f.run(t, true, false)
			req.NoError(err)
			check.Equal("other", f.body(t, "$other"))
			check.Equal("older", f.body(t, "$original"))
		})
	}
}

func TestImportRecoveryPrefersOriginalBeforeCopiedCiphertext(t *testing.T) {
	for _, mode := range []string{"backfill", "copy recovered in earlier sync", "equal timestamp", "recovery page boundary"} {
		t.Run(mode, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			original := f.message(t, "$a-original", "original sender's text", 10)
			replayed := *original
			replayed.ID, replayed.Sender, replayed.Timestamp = "$z-replayed", "@other:example.org", 20
			if mode == "equal timestamp" {
				replayed.Timestamp = original.Timestamp
			}
			f.timeline = []*event.Event{&replayed}
			if mode == "recovery page boundary" {
				for i := range 499 {
					missing := f.message(t, fmt.Sprintf("$unavailable-%d", i), "unavailable", 30)
					missing.Content.AsEncrypted().SessionID = "missing"
					f.timeline = append(f.timeline, missing)
				}
			}
			if mode == "copy recovered in earlier sync" {
				_, err := f.run(t, true, false)
				req.NoError(err)
			}
			handler := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/messages") {
					writeMatrixJSON(t, w, map[string]any{"chunk": []*event.Event{original}, "end": ""})
					return
				}
				handler.ServeHTTP(w, r)
			})
			_, err := f.run(t, true, mode == "copy recovered in earlier sync")
			req.NoError(err)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$a-original", "$z-replayed"})
			req.NoError(err)
			req.Less(ids["$z-replayed"], ids["$a-original"], "the copy must have the earlier archive row")
			check.Equal("original sender's text", f.body(t, "$a-original"))
			check.Equal(encryptedPlaceholder, f.body(t, "$z-replayed"))
			raw, err := f.store.GetMessageRaw(ids["$z-replayed"])
			req.NoError(err)
			wantRaw, err := json.Marshal(&replayed)
			req.NoError(err)
			check.JSONEq(string(wantRaw), string(raw), "restore the losing event's original ciphertext")
			metadata, err := f.store.GetMessageMetadata(ids["$z-replayed"])
			req.NoError(err)
			var conflict struct {
				Winner string `json:"matrix_replay_conflict"`
			}
			req.NoError(json.Unmarshal([]byte(metadata.String), &conflict))
			check.Equal("$a-original", conflict.Winner)
			if !f.store.IsPostgreSQL() {
				var indexed, matches int64
				req.NoError(f.store.DB().QueryRow(`SELECT MIN(rowid), COUNT(*) FROM messages_fts WHERE messages_fts MATCH '"sender"'`).Scan(&indexed, &matches))
				check.Equal(int64(1), matches)
				check.Equal(ids["$a-original"], indexed, "search must attribute the text to its original event")
			}
			f.quiet = true
			_, err = f.run(t, true, true)
			req.NoError(err)
			check.Equal("original sender's text", f.body(t, "$a-original"))
			check.Equal(encryptedPlaceholder, f.body(t, "$z-replayed"))
		})
	}
}

func TestImportRecoveryKeepsCopyWithoutPreservedCiphertext(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	original := f.message(t, "$a-original", "original sender's text", 10)
	replayed := *original
	replayed.ID, replayed.Sender, replayed.Timestamp = "$z-replayed", "@other:example.org", 20
	f.timeline = []*event.Event{&replayed}
	_, err := f.run(t, true, false)
	req.NoError(err)
	// Rows recovered before the ciphertext was preserved cannot be returned to a placeholder.
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$z-replayed"})
	req.NoError(err)
	raw, err := f.store.GetMessageRaw(ids["$z-replayed"])
	req.NoError(err)
	var recovered map[string]any
	req.NoError(json.Unmarshal(raw, &recovered))
	req.Contains(recovered, "org.msgvault.encrypted_event")
	delete(recovered, "org.msgvault.encrypted_event")
	raw, err = json.Marshal(recovered)
	req.NoError(err)
	req.NoError(f.store.UpsertMessageRawWithFormat(ids["$z-replayed"], raw, rawFormat))
	handler := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			writeMatrixJSON(t, w, map[string]any{"chunk": []*event.Event{original}, "end": ""})
			return
		}
		handler.ServeHTTP(w, r)
	})
	for range 2 {
		_, err = f.run(t, true, true)
		req.NoError(err, "a copy without its ciphertext must not fail every sync")
	}
	check.Equal("original sender's text", f.body(t, "$z-replayed"))
	check.Equal(encryptedPlaceholder, f.body(t, "$a-original"))
}

func TestImportPendingEditsRejectOtherSendersAndKeepNewestBounded(t *testing.T) {
	for _, sender := range []id.UserID{"@other:example.org", "@member:example.org"} {
		t.Run(sender.String(), func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			f.timeline = []*event.Event{f.message(t, "$original", "original", 1)}
			_, err := f.run(t, false, false)
			req.NoError(err)
			f.timeline = nil
			for i := range 40 {
				f.timeline = append(f.timeline, &event.Event{
					ID: id.EventID(fmt.Sprintf("$edit-%02d", i)), Sender: sender, Timestamp: int64(100 + i), Type: event.EventMessage,
					Content: event.Content{Parsed: &event.MessageEventContent{
						MsgType: event.MsgText, Body: "* edited",
						NewContent: &event.MessageEventContent{MsgType: event.MsgText, Body: fmt.Sprintf("edit %d", i)},
						RelatesTo:  &event.RelatesTo{Type: event.RelReplace, EventID: "$original"},
					}},
				})
			}
			_, err = f.run(t, false, false)
			req.NoError(err)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$original"})
			req.NoError(err)
			metadata, err := f.store.GetMessageMetadata(ids["$original"])
			req.NoError(err)
			var pending struct {
				Edits []*event.Event `json:"matrix_pending_edits"`
			}
			req.NoError(json.Unmarshal([]byte(metadata.String), &pending))
			wantBody := "original"
			if sender == "@other:example.org" {
				check.Empty(pending.Edits)
			} else {
				req.Len(pending.Edits, 32)
				check.Equal(id.EventID("$edit-08"), pending.Edits[0].ID)
				check.Equal(id.EventID("$edit-39"), pending.Edits[31].ID)
				f.timeline = []*event.Event{f.timeline[0], f.timeline[39]}
				_, err = f.run(t, false, false)
				req.NoError(err)
				after, err := f.store.GetMessageMetadata(ids["$original"])
				req.NoError(err)
				check.JSONEq(metadata.String, after.String, "old and duplicate events must not change the bounded queue")
				f.timeline = []*event.Event{{ID: "$redaction", Type: event.EventRedaction, Redacts: "$edit-39", Content: event.Content{Raw: map[string]any{}}}}
				_, err = f.run(t, false, false)
				req.NoError(err)
				wantBody = "edit 38"
			}
			f.quiet = true
			_, err = f.run(t, true, false)
			req.NoError(err)
			check.Equal(wantBody, f.body(t, "$original"))
		})
	}
}
