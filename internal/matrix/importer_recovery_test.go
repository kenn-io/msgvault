package matrix

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
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
	left       bool
	extraRooms map[string][]*event.Event
	nextBatch  string
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
		if f.left {
			rooms["leave"] = map[string]any{"!room:example.org": map[string]any{"timeline": map[string]any{"events": []*event.Event{}}}}
		}
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
	req.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions`).Scan(&reactions))
	check.Equal(1, reactions)
	f.timeline = append(f.timeline, &event.Event{ID: "$redaction", Type: event.EventRedaction, Redacts: "$reaction", Content: event.Content{Raw: map[string]any{}}})
	_, err = f.run(t, false, true)
	req.NoError(err)
	req.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions`).Scan(&reactions))
	check.Zero(reactions)
	f.quiet = true
	_, err = f.run(t, true, false)
	req.NoError(err)
	req.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions`).Scan(&reactions))
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
		check.Empty(f.pendingEdits(t, "$z-original"))
	})
}

func (f *recoveryFixture) body(t *testing.T, eventID string) string {
	t.Helper()
	found, err := f.store.MessageExistsBatch(f.sourceID, []string{eventID})
	require.NoError(t, err)
	require.NotZero(t, found[eventID])
	body, err := f.store.GetMessageBodyText(t.Context(), found[eventID])
	require.NoError(t, err)
	return body
}

func (f *recoveryFixture) operation(t *testing.T) operations.Run {
	t.Helper()
	req := require.New(t)
	last, err := f.store.GetLastSuccessfulSyncByType(t.Context(), f.sourceID, SourceType)
	req.NoError(err)
	id, err := operations.NewInt64ID(operations.KindSourceSync, last.ID)
	req.NoError(err)
	run, err := f.store.GetRun(t.Context(), id)
	req.NoError(err)
	return run
}

// interrupt makes writes matching when fail until the returned function is called.
func (f *recoveryFixture) interrupt(t *testing.T, operation, table, record, when string) func() {
	t.Helper()
	req := require.New(t)
	if f.store.IsPostgreSQL() {
		_, err := f.store.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE FUNCTION stop_recovery_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF %s THEN RAISE EXCEPTION 'interrupted recovery'; END IF; RETURN %s; END $$`, when, record))
		req.NoError(err)
		_, err = f.store.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER stop_recovery BEFORE %s ON %s FOR EACH ROW EXECUTE FUNCTION stop_recovery_fn()`, operation, table))
		req.NoError(err)
		return func() {
			_, err := f.store.DB().ExecContext(t.Context(), `DROP TRIGGER stop_recovery ON `+table)
			req.NoError(err)
			_, err = f.store.DB().ExecContext(t.Context(), `DROP FUNCTION stop_recovery_fn()`)
			req.NoError(err)
		}
	}
	_, err := f.store.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER stop_recovery BEFORE %s ON %s WHEN %s BEGIN SELECT RAISE(ABORT, 'interrupted recovery'); END`, operation, table, when))
	req.NoError(err)
	return func() {
		_, err := f.store.DB().ExecContext(t.Context(), `DROP TRIGGER stop_recovery`)
		req.NoError(err)
	}
}

// ageAttempts moves every failed attempt past the retry cooldown.
func (f *recoveryFixture) ageAttempts(t *testing.T) {
	t.Helper()
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE matrix_encrypted_events SET tried_at = ? WHERE tried_at IS NOT NULL`), time.Now().Add(-2*recoveryCooldown).Unix())
	require.NoError(t, err)
}

func (f *recoveryFixture) pendingEdits(t *testing.T, target string) []string {
	t.Helper()
	req := require.New(t)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{target})
	req.NoError(err)
	rows, err := f.store.DB().QueryContext(t.Context(), f.store.Rebind(`SELECT event_id FROM matrix_pending_edits WHERE target_message_id = ? ORDER BY origin_ts, event_id`), ids[target])
	req.NoError(err)
	defer func() { _ = rows.Close() }()
	var edits []string
	for rows.Next() {
		var edit string
		req.NoError(rows.Scan(&edit))
		edits = append(edits, edit)
	}
	req.NoError(rows.Err())
	return edits
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
	req.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), ids["$original"]).Scan(&deleted))
	check.True(deleted)
	f.quiet = false
	f.timeline = []*event.Event{f.message(t, "$target", "target", 10)}
	_, err = f.run(t, true, false)
	req.NoError(err)
	ids, err = f.store.MessageExistsBatch(f.sourceID, []string{"$original", "$target"})
	req.NoError(err)
	var targetID int64
	req.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), ids["$original"]).Scan(&targetID))
	check.Equal(ids["$target"], targetID)
}

func TestImportRecoveryFinishesInterruptedWrites(t *testing.T) {
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
			resume := f.interrupt(t, operation, "messages", record, fmt.Sprintf("%s.source_message_id = '%s'", record, stopID))
			f.quiet = true
			sum, err := f.run(t, true, false)
			req.ErrorContains(err, "interrupted recovery")
			wantPending := int64(1)
			if mode == "between originals" {
				wantPending = 2 // The reaction waits for the page's messages.
			}
			check.Equal(wantPending, sum.PendingEncrypted, "the interrupted event stays a placeholder")
			check.Equal(encryptedPlaceholder, f.body(t, stopID))
			resume()
			sum, err = f.run(t, true, false)
			req.NoError(err)
			check.Zero(sum.PendingEncrypted)
			check.Equal("original", f.body(t, "$original"))
			wantReactions, wantRows := 2, 1
			if mode == "between originals" {
				check.Equal("second", f.body(t, "$second"))
				wantReactions, wantRows = 1, 2
			}
			var reactions int
			req.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions`).Scan(&reactions))
			check.Equal(wantReactions, reactions)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$original", "$second", "$reaction", "$second-reaction"})
			req.NoError(err)
			check.Len(ids, wantRows, "recovered reactions leave no placeholder rows")
		})
	}
}

func TestImportRecoveryKeepsEditsAppliedBeforeInterruption(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{f.message(t, "$original", "original", 10)}
	_, err := f.run(t, false, false)
	req.NoError(err)
	f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":20,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`)}
	_, err = f.run(t, false, false)
	req.NoError(err)
	req.Equal([]string{"$edit"}, f.pendingEdits(t, "$original"))
	// Stop after the content and its queued edit are written, before the placeholder is marked recovered.
	resume := f.interrupt(t, "UPDATE", "matrix_encrypted_events", "NEW", "NEW.recovered")
	f.quiet = true
	_, err = f.run(t, true, false)
	req.ErrorContains(err, "interrupted recovery")
	resume()
	sum, err := f.run(t, true, false)
	req.NoError(err)
	check.Zero(sum.PendingEncrypted)
	check.Equal("edited", f.body(t, "$original"))
	check.Empty(f.pendingEdits(t, "$original"))
}

func TestImportRecoversPlaceholderFromInterruptedRun(t *testing.T) {
	for _, mode := range []string{"placeholder arrives in same sync", "quiet follow-up sync"} {
		t.Run(mode, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			missing := f.message(t, "$missing", "missing", 10)
			missing.Content.AsEncrypted().SessionID = "absent"
			f.timeline = []*event.Event{missing}
			_, err := f.run(t, true, false)
			req.NoError(err)
			convID, err := f.store.EnsureConversation(t.Context(), f.sourceID, "!room:example.org", "")
			req.NoError(err)
			fresh := f.message(t, "$fresh", "fresh", 20)
			fresh.RoomID = "!room:example.org"
			// Commit the placeholder, then discard the importer before it recovers anything.
			req.NoError(NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).persistEvent(t.Context(), f.sourceID, convID, fresh, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
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

func TestImportRecoveryRetriesAfterBackupChangeAndCooldown(t *testing.T) {
	t.Run("missing sessions", func(t *testing.T) {
		req, check := require.New(t), assert.New(t)
		f := newRecoveryFixture(t)
		f.extraRooms = map[string][]*event.Event{}
		const roomCount = 3
		for i := range roomCount {
			roomID := fmt.Sprintf("!missing%d:example.org", i)
			evt := f.message(t, "$missing"+strconv.Itoa(i), "missing", 10)
			evt.RoomID = id.RoomID(roomID)
			evt.Content.AsEncrypted().SessionID = "absent"
			f.extraRooms[roomID] = []*event.Event{evt}
		}
		_, err := f.run(t, true, false)
		req.NoError(err)
		check.Equal(roomCount, f.backup.roomFetches)
		f.quiet = true
		_, err = f.run(t, true, false)
		req.NoError(err)
		check.Equal(roomCount, f.backup.roomFetches, "an unchanged backup is not read again")
		f.backup.etag = "2"
		_, err = f.run(t, true, false)
		req.NoError(err)
		check.Equal(roomCount, f.backup.roomFetches, "the cooldown holds even when the backup changes")
		f.ageAttempts(t)
		_, err = f.run(t, true, false)
		req.NoError(err)
		check.Equal(2*roomCount, f.backup.roomFetches, "a changed backup is read after the cooldown")
		f.ageAttempts(t)
		_, err = f.run(t, true, false)
		req.NoError(err)
		check.Equal(2*roomCount, f.backup.roomFetches, "an unchanged backup is not read again after the cooldown")
		f.quiet = false
		f.timeline = []*event.Event{f.message(t, "$new", "new", 20)}
		_, err = f.run(t, true, false)
		req.NoError(err)
		check.Equal("new", f.body(t, "$new"))
		check.Equal(2*roomCount+1, f.backup.roomFetches, "a new event is tried at once, without the old rooms")
		_, err = f.run(t, true, true)
		req.NoError(err)
		check.Equal(3*roomCount+1, f.backup.roomFetches, "a full sync tries every pending room")
	})
	t.Run("failed room fetch", func(t *testing.T) {
		req, check := require.New(t), assert.New(t)
		f := newRecoveryFixture(t)
		f.timeline = []*event.Event{f.message(t, "$pending", "pending", 10)}
		_, err := f.run(t, false, false)
		req.NoError(err)
		f.quiet, f.backup.status = true, http.StatusServiceUnavailable
		_, err = f.run(t, true, false)
		req.ErrorContains(err, "fetch Matrix room backup")
		check.Equal(1, f.backup.roomFetches)
		f.backup.status = 0
		_, err = f.run(t, true, false)
		req.NoError(err)
		check.Equal(2, f.backup.roomFetches, "a failed read retries on the next sync without a backup change")
		check.Equal("pending", f.body(t, "$pending"))
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
}

func TestImportRecoversPlaceholderThatReusesCollectedID(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	if f.store.IsPostgreSQL() {
		t.Skip("requires SQLite rowid reuse")
	}
	missing := f.message(t, "$missing", "missing", 10)
	missing.Content.AsEncrypted().SessionID = "absent"
	f.timeline = []*event.Event{missing, f.message(t, "$deleted", "deleted", 20)}
	_, err := f.run(t, false, false)
	req.NoError(err)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$deleted"})
	req.NoError(err)
	req.NoError(f.store.MarkMessageDeleted(f.sourceID, "$deleted"))
	plan, err := f.store.PlanGCContext(t.Context())
	req.NoError(err)
	collected, err := f.store.ExecuteGCContext(t.Context(), plan)
	req.NoError(err)
	req.Equal(int64(1), collected)
	f.timeline = []*event.Event{f.message(t, "$new", "new", 30)}
	_, err = f.run(t, false, false)
	req.NoError(err)
	reused, err := f.store.MessageExistsBatch(f.sourceID, []string{"$new"})
	req.NoError(err)
	req.Equal(ids["$deleted"], reused["$new"], "SQLite reuses the collected table-max rowid")
	f.quiet = true
	sum, err := f.run(t, true, false)
	req.NoError(err)
	check.Equal(int64(1), sum.Recovered)
	check.Equal(int64(1), sum.PendingEncrypted)
	check.Equal("new", f.body(t, "$new"))
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
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$unsupported", "$malformed"})
	req.NoError(err)
	check.Zero(ids["$unsupported"], "an unsupported event leaves no placeholder")
	check.Equal(encryptedPlaceholder, f.body(t, "$malformed"))
}

func TestImportPendingRepliesCrossPagesWithoutRawEvents(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	convID, err := f.store.EnsureConversation(t.Context(), f.sourceID, "!room:example.org", "Room")
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
		req.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), messageID).Scan(&targetID))
		check.Equal(targets["$target"], targetID.Int64)
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
					req.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions`).Scan(&recovered))
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
				check.False(metadata.Valid, "a replayed copy is never recovered")
				switch kind {
				case "reaction":
					req.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions`).Scan(&recovered))
					check.Equal(1, recovered)
					req.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reaction_source_events WHERE source_reaction_id = '$second'`).Scan(&recovered))
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

func TestEditRollbackPrefersEarlierArchivedCiphertext(t *testing.T) {
	for _, mode := range []string{"same", "alternate base64", "encrypted target", "stripped relation", "invalid earlier copy", "later copy", "already claimed"} {
		t.Run(mode, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			original := f.message(t, "$original", "original", 10)
			initialKey := f.backup.data.SessionKey
			if mode == "encrypted target" {
				in, err := olm.NewInboundGroupSession([]byte(f.backup.out.Key()))
				req.NoError(err)
				exported, err := in.Export(1)
				req.NoError(err)
				f.backup.data.SessionKey = string(exported)
			}
			other := f.message(t, "$other", "other", 11)
			index := f.backup.out.MessageIndex()
			edit := f.edit(t, "$edit", "$original", "edited", 20)
			eligible := mode == "invalid earlier copy" || mode == "later copy" || mode == "already claimed"
			target := "$other"
			if eligible {
				target = "$original"
			}
			newest := f.edit(t, "$newest", target, "newest", 30)
			f.timeline = []*event.Event{original, other, newest}
			if mode == "already claimed" {
				f.timeline = append(f.timeline, edit)
			}
			_, err := f.run(t, true, false)
			req.NoError(err)

			raw, err := json.Marshal(edit)
			req.NoError(err)
			copied := matrixTestEvent(t, string(raw))
			copied.ID, copied.Timestamp = "$replayed", 25
			req.True(parseContent(copied))
			pending, candidate := edit, copied
			wantOriginal, wantOwner := "edited", "$edit"
			if eligible {
				pending, candidate = copied, edit
				copied.ID = "$copy"
				copied.Timestamp = 15
				if mode == "invalid earlier copy" {
					copied.Sender = "@other:example.org"
				}
				if mode == "later copy" {
					copied.Timestamp = 40
				}
			} else {
				copied.Content.AsEncrypted().RelatesTo.EventID = "$other"
				if mode == "stripped relation" {
					edit.Content.AsEncrypted().RelatesTo = nil
					wantOriginal = "original"
				}
				if mode == "alternate base64" {
					ciphertext := copied.Content.AsEncrypted().MegolmCiphertext
					decoded, err := base64.RawStdEncoding.DecodeString(string(ciphertext))
					req.NoError(err)
					alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
					ciphertext[len(ciphertext)-1] = alphabet[strings.IndexByte(alphabet, ciphertext[len(ciphertext)-1])^1]
					alternate, err := base64.RawStdEncoding.DecodeString(string(ciphertext))
					req.NoError(err)
					req.Equal(decoded, alternate)
				}
			}
			f.timeline = []*event.Event{pending}
			_, err = f.run(t, false, false)
			req.NoError(err)
			check.Equal(encryptedPlaceholder, f.body(t, pending.ID.String()))

			f.edits = []*event.Event{candidate}
			f.timeline = []*event.Event{{ID: "$redaction", Type: event.EventRedaction, Redacts: "$newest", Content: event.Content{Raw: map[string]any{}}}}
			_, err = f.run(t, true, false)
			req.NoError(err)
			check.Equal("other", f.body(t, "$other"))
			var owner string
			req.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT event_id FROM matrix_megolm_claims WHERE session_id = ? AND message_index = ?`), f.backup.out.ID().String(), index).Scan(&owner))
			check.Equal(wantOwner, owner)
			if mode == "stripped relation" {
				check.Equal("* edited", f.body(t, "$edit"))
			}
			if mode == "encrypted target" {
				check.Equal([]string{"$edit"}, f.pendingEdits(t, "$original"))
				f.quiet, f.backup.data.SessionKey, f.backup.etag = true, initialKey, "2"
				_, err = f.run(t, true, true)
				req.NoError(err)
				check.Equal("other", f.body(t, "$other"))
			}
			check.Equal(wantOriginal, f.body(t, "$original"))
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
				req.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT MIN(rowid), COUNT(*) FROM messages_fts WHERE messages_fts MATCH '"sender"'`).Scan(&indexed, &matches))
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

func TestImportRecoveryFinishesInterruptedReplayFix(t *testing.T) {
	for _, mode := range []string{"before the copy is rewritten", "before the claim moves"} {
		t.Run(mode, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			original := f.message(t, "$a-original", "original sender's text", 10)
			replayed := *original
			replayed.ID, replayed.Sender, replayed.Timestamp = "$z-replayed", "@other:example.org", 20
			f.timeline = []*event.Event{&replayed}
			_, err := f.run(t, true, false)
			req.NoError(err)
			req.Equal("original sender's text", f.body(t, "$z-replayed"))
			handler := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/messages") {
					writeMatrixJSON(t, w, map[string]any{"chunk": []*event.Event{original}, "end": ""})
					return
				}
				handler.ServeHTTP(w, r)
			})
			table, when := "matrix_megolm_claims", "NEW.event_id = '$a-original'"
			if mode == "before the copy is rewritten" {
				table, when = "messages", "OLD.source_message_id = '$z-replayed'"
			}
			resume := f.interrupt(t, "UPDATE", table, "NEW", when)
			_, err = f.run(t, true, true)
			req.ErrorContains(err, "interrupted recovery")
			resume()
			for range 2 {
				_, err = f.run(t, true, false)
				req.NoError(err)
				check.Equal("original sender's text", f.body(t, "$a-original"))
				check.Equal(encryptedPlaceholder, f.body(t, "$z-replayed"))
			}
		})
	}
}

func TestImportRecoveryAppliesOneCopyOfAReplayedReaction(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	original := f.backup.encrypted(t, "$a-reaction", 20, event.EventReaction, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$target", "key": "yes"}})
	copied := *original
	copied.ID, copied.Sender, copied.Timestamp = "$z-copy", "@other:example.org", 30
	// The later copy gets the lower row ID, so recovery reaches it first.
	f.timeline = []*event.Event{f.message(t, "$target", "target", 10), &copied, original}
	_, err := f.run(t, false, false)
	req.NoError(err)
	f.quiet = true
	_, err = f.run(t, true, false)
	req.NoError(err)
	var reactions int
	req.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions`).Scan(&reactions))
	check.Equal(1, reactions, "a reaction's Megolm index applies once")
}

func TestImportRecoveryKeepsEditClaimFromStrippedCopy(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	edit := f.edit(t, "$edit", "$target", "edited", 20)
	content := *edit.Content.AsEncrypted()
	content.RelatesTo = nil
	// A backdated copy whose outer relation was removed decrypts as a message.
	copied := *edit
	copied.ID, copied.Sender, copied.Timestamp, copied.Content = "$copy", "@other:example.org", 5, event.Content{Parsed: &content}
	f.timeline = []*event.Event{f.message(t, "$target", "target", 10), edit, &copied}
	_, err := f.run(t, false, false)
	req.NoError(err)
	f.quiet = true
	_, err = f.run(t, true, false)
	req.NoError(err)
	check.Equal("edited", f.body(t, "$target"))
	check.Equal(encryptedPlaceholder, f.body(t, "$copy"), "the copy must not take the edit's Megolm index")
}

func TestImportRecoveryReturnsAnnotatedMessageCopyToPlaceholder(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	original := f.message(t, "$a-original", "original sender's text", 10)
	content := *original.Content.AsEncrypted()
	content.RelatesTo = &event.RelatesTo{Type: event.RelAnnotation, EventID: "$a-original", Key: "yes"}
	// A later copy with an added annotation still decrypts as a message.
	copied := *original
	copied.ID, copied.Sender, copied.Timestamp, copied.Content = "$z-copy", "@other:example.org", 20, event.Content{Parsed: &content}
	f.timeline = []*event.Event{&copied}
	_, err := f.run(t, true, false)
	req.NoError(err)
	f.timeline = []*event.Event{original}
	_, err = f.run(t, true, false)
	req.NoError(err)
	check.Equal("original sender's text", f.body(t, "$a-original"))
	check.Equal(encryptedPlaceholder, f.body(t, "$z-copy"))
}

func TestImportRecoveryFinishesReplyLinkAfterInterruption(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$target","sender":"@member:example.org","origin_server_ts":10,"content":{"msgtype":"m.text","body":"target"}}`),
		f.backup.encrypted(t, "$reply", 20, event.EventMessage, map[string]any{"msgtype": "m.text", "body": "reply", "m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": "$target"}}}),
	}
	_, err := f.run(t, false, false)
	req.NoError(err)
	// Stop after the reply's text is written, before its reply link.
	resume := f.interrupt(t, "UPDATE OF reply_to_message_id", "messages", "NEW", "NEW.reply_to_message_id IS NOT NULL")
	f.quiet = true
	_, err = f.run(t, true, false)
	req.ErrorContains(err, "interrupted recovery")
	check.Equal("reply", f.body(t, "$reply"))
	resume()
	sum, err := f.run(t, true, false)
	req.NoError(err)
	check.Zero(sum.PendingEncrypted)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$reply", "$target"})
	req.NoError(err)
	var replyTo int64
	req.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), ids["$reply"]).Scan(&replyTo))
	check.Equal(ids["$target"], replyTo)
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
			pending := f.pendingEdits(t, "$original")
			wantBody := "original"
			if sender == "@other:example.org" {
				check.Empty(pending)
			} else {
				req.Len(pending, maxPendingEdits)
				check.Equal("$edit-08", pending[0])
				check.Equal("$edit-39", pending[maxPendingEdits-1])
				f.timeline = []*event.Event{f.timeline[0], f.timeline[39]}
				_, err = f.run(t, false, false)
				req.NoError(err)
				check.Equal(pending, f.pendingEdits(t, "$original"), "old and duplicate events must not change the bounded queue")
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

func TestImportRecoveryMediaSurvivesCancellation(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{f.backup.encrypted(t, "$photo", 10, event.EventMessage, map[string]any{"msgtype": "m.image", "body": "photo.jpg", "url": "mxc://example.org/photo"})}
	_, err := f.run(t, false, false)
	req.NoError(err)
	f.quiet, f.left = true, true
	mux, ok := f.server.Config.Handler.(*http.ServeMux)
	req.True(ok)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	interrupt := true
	observed := make(chan struct {
		body string
		err  error
	}, 1)
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/photo", func(w http.ResponseWriter, r *http.Request) {
		if interrupt {
			probe, done := context.WithTimeout(t.Context(), 5*time.Second)
			defer done()
			var body string
			err := f.store.DB().QueryRowContext(probe, f.store.Rebind(`SELECT snippet FROM messages WHERE source_id = ? AND source_message_id = ?`), f.sourceID, "$photo").Scan(&body)
			observed <- struct {
				body string
				err  error
			}{body, err}
			w.WriteHeader(http.StatusOK)
			_ = http.NewResponseController(w).Flush()
			cancel()
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte("photo bytes"))
	})
	opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
	leased := false
	opts.MediaMutation = func(_ context.Context, write func() error) error {
		check.False(leased, "media writes must not nest the maintenance lease")
		leased = true
		defer func() { leased = false }()
		if err := write(); err != nil {
			return err
		}
		probe, done := context.WithTimeout(t.Context(), 5*time.Second)
		defer done()
		var body string
		err := f.store.DB().QueryRowContext(probe, f.store.Rebind(`SELECT snippet FROM messages WHERE source_id = ? AND source_message_id = ?`), f.sourceID, "$photo").Scan(&body)
		check.Equal("[image]", body, "the recovered message must be written before its media")
		return err
	}
	sum, err := NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(ctx, opts)
	req.ErrorIs(err, context.Canceled)
	result := <-observed
	req.NoError(result.err)
	check.Equal("[image]", result.body, "recovery must commit before media download")
	check.Equal(int64(1), sum.Recovered)
	check.Equal("[image]", f.body(t, "$photo"))
	interrupt = false
	sum, err = NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(t.Context(), opts)
	req.NoError(err)
	check.Zero(sum.Recovered)
	check.Equal(int64(1), sum.AttachmentsDownloaded)
}

func TestImportRecoversRoomMedia(t *testing.T) {
	for _, mode := range []string{"download", "no-media"} {
		t.Run(mode, func(t *testing.T) {
			req := require.New(t)
			check := assert.New(t)
			f := newRecoveryFixture(t)
			mux, ok := f.server.Config.Handler.(*http.ServeMux)
			req.True(ok)
			mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/photo", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("photo bytes"))
			})
			content := map[string]any{"body": "photo.jpg", "url": "mxc://example.org/photo", "info": map[string]any{"mimetype": "image/jpeg", "size": 11}}
			content["msgtype"] = "m.image"
			f.timeline = []*event.Event{f.backup.encrypted(t, "$photo", 10, event.EventMessage, content)}
			_, err := f.run(t, false, false)
			req.NoError(err)
			check.Equal(encryptedPlaceholder, f.body(t, "$photo"))
			f.quiet, f.left = true, true
			opts := ImportOptions{UserID: "@archive:example.org", NoMedia: mode == "no-media", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxParticipants: 20, MaxBytes: attachmentpolicy.DefaultChatMaxBytes}}
			sum, err := NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(t.Context(), opts)
			req.NoError(err)
			check.Equal(int64(1), sum.Recovered)
			if mode != "download" {
				check.Zero(sum.AttachmentsDownloaded)
				check.Equal(int64(1), sum.AttachmentsPending)
				check.Zero(sum.AttachmentsFailed)
				opts.NoMedia = false
				opts.ExcludeRooms = []string{"!room:example.org"}
				sum, err = NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(t.Context(), opts)
				req.NoError(err)
				check.Zero(sum.AttachmentsDownloaded)
				opts.ExcludeRooms = nil
				opts.Rooms = []string{"!other:example.org"}
				sum, err = NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(t.Context(), opts)
				req.NoError(err)
				check.Zero(sum.AttachmentsDownloaded)
				opts.Rooms = []string{"!room:example.org"}
				sum, err = NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(t.Context(), opts)
				req.NoError(err)
				check.Zero(sum.Recovered)
			}
			check.Equal(int64(1), sum.AttachmentsDownloaded)
			check.Zero(sum.PendingEncrypted)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$photo"})
			req.NoError(err)
			refs, err := f.store.MessageMatrixAttachments(t.Context(), ids["$photo"])
			req.NoError(err)
			ref := refs["matrix:mxc://example.org/photo"]
			check.Equal(attachmentpolicy.StateStored, ref.State)
			bytes, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, ref.StoragePath))
			req.NoError(err)
			check.Equal("photo bytes", string(bytes))
		})
	}
}

func TestImportRecoveryEarlierMessageTakesMediaFromCopy(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	mux, ok := f.server.Config.Handler.(*http.ServeMux)
	req.True(ok)
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/photo", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("photo bytes"))
	})
	original := f.backup.encrypted(t, "$a-original", 10, event.EventMessage, map[string]any{"msgtype": "m.image", "body": "photo.jpg", "url": "mxc://example.org/photo"})
	replayed := *original
	replayed.ID, replayed.Sender, replayed.Timestamp = "$z-replayed", "@other:example.org", 20
	f.timeline = []*event.Event{&replayed}
	opts := ImportOptions{UserID: "@archive:example.org", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
	_, err := NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(t.Context(), opts)
	req.NoError(err)
	media := func(eventID string) map[string]store.AttachmentRef {
		t.Helper()
		ids, err := f.store.MessageExistsBatch(f.sourceID, []string{eventID})
		req.NoError(err)
		refs, err := f.store.MessageMatrixAttachments(t.Context(), ids[eventID])
		req.NoError(err)
		return refs
	}
	req.Equal(attachmentpolicy.StateStored, media("$z-replayed")["matrix:mxc://example.org/photo"].State)

	// Backfill then archives the original event, which takes the index back.
	handler := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			writeMatrixJSON(t, w, map[string]any{"chunk": []*event.Event{original}, "end": ""})
			return
		}
		handler.ServeHTTP(w, r)
	})
	opts.Full = true
	_, err = NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(t.Context(), opts)
	req.NoError(err)
	check.Equal(encryptedPlaceholder, f.body(t, "$z-replayed"))
	check.Empty(media("$z-replayed"))
	check.Equal(attachmentpolicy.StateStored, media("$a-original")["matrix:mxc://example.org/photo"].State)
}

func TestImportRecordsMediaAnInterruptedWriteLeftOut(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/file"}}`)}
	opts := ImportOptions{UserID: "@archive:example.org", NoMedia: true, AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
	_, err := NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(t.Context(), opts)
	req.NoError(err)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$media"})
	req.NoError(err)
	// The message write committed and the run stopped before its media write.
	req.NoError(f.store.ReplaceMessageMatrixAttachments(t.Context(), ids["$media"], nil))

	_, err = NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(t.Context(), opts)
	req.NoError(err)
	refs, err := f.store.MessageMatrixAttachments(t.Context(), ids["$media"])
	req.NoError(err)
	check.Equal(attachmentpolicy.StatePending, refs["matrix:mxc://example.org/file"].State)
}
