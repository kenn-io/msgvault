package matrix

import (
	"context"
	"database/sql"
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
		check.False(leased, "recovered media must share the recovery transaction's lease")
		leased = true
		defer func() { leased = false }()
		if err := write(); err != nil {
			return err
		}
		probe, done := context.WithTimeout(t.Context(), 5*time.Second)
		defer done()
		var body string
		err := f.store.DB().QueryRowContext(probe, f.store.Rebind(`SELECT snippet FROM messages WHERE source_id = ? AND source_message_id = ?`), f.sourceID, "$photo").Scan(&body)
		check.Equal("[image]", body, "recovery must commit before releasing the mutation lease")
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

type recoveryFixture struct {
	backup       *backupFixture
	store        *store.Store
	sourceID     int64
	server       *httptest.Server
	timeline     []*event.Event
	history      []*event.Event
	edits        []*event.Event
	quiet        bool
	left         bool
	historyFails bool
	extraRooms   map[string][]*event.Event
	nextBatch    string
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

func TestImportRecoveryDecryptsBeforeTakingWriteLock(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	otherRoom, err := f.store.EnsureConversation(f.sourceID, "!other:example.org", "Other")
	req.NoError(err)
	f.timeline = []*event.Event{f.message(t, "$original", "original", 10)}
	_, err = f.run(t, false, false)
	req.NoError(err)
	r := backupRuntime(t, f.server.URL, f.backup)
	req.NoError(r.prepareBackup(t.Context()))
	req.NoError(r.fetchRoomBackup(t.Context(), "!room:example.org"))
	key := backupSessionKey{"!room:example.org", f.backup.out.ID()}
	r.cacheSession(key, r.sessionData[key])
	req.NotNil(r.sessions[key])
	var ingestErr error
	r.sessions[key] = ingestDuringDecryption{r.sessions[key], func() error {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_, ingestErr = f.store.PersistMessageContext(ctx, &store.MessagePersistData{Message: &store.Message{SourceID: f.sourceID, ConversationID: otherRoom, SourceMessageID: "$other-ingest", MessageType: SourceType}})
		return ingestErr
	}}
	rooms, err := f.store.MatrixPlaceholderRooms(t.Context(), f.sourceID, nil, nil, attachmentpolicy.Policy{})
	req.NoError(err)
	req.Len(rooms, 1)
	sum := &ImportSummary{PendingEncrypted: 1}
	retired, err := NewImporter(f.store, r).recoverRoom(t.Context(), f.sourceID, rooms[0], 0, ImportOptions{}, sum)
	req.NoError(err)
	check.NoError(ingestErr)
	check.Equal(int64(1), retired)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$other-ingest"})
	req.NoError(err)
	check.NotZero(ids["$other-ingest"])
	check.Equal(int64(1), sum.Recovered)
	check.Zero(sum.PendingEncrypted)
	check.Equal("original", f.body(t, "$original"))
}

func TestImportRecoveryWithoutDecryptedItemsAvoidsWriteLock(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	missing := f.message(t, "$missing", "missing", 10)
	missing.Content.AsEncrypted().SessionID = "absent"
	f.timeline = []*event.Event{missing}
	_, err := f.run(t, false, false)
	req.NoError(err)
	rooms, err := f.store.MatrixPlaceholderRooms(t.Context(), f.sourceID, nil, nil, attachmentpolicy.Policy{})
	req.NoError(err)
	req.Len(rooms, 1)
	r := backupRuntime(t, f.server.URL, f.backup)
	sum := &ImportSummary{PendingEncrypted: 1}
	// An empty recovery page must finish while another transaction holds the write lock.
	req.NoError(f.store.MatrixRecoveryTransaction(t.Context(), func(_ *store.Store) error {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		retired, err := NewImporter(f.store, r).recoverRoom(ctx, f.sourceID, rooms[0], 0, ImportOptions{}, sum)
		check.Zero(retired)
		return err
	}))
	check.Equal(int64(1), sum.PendingEncrypted)
	check.Zero(sum.Recovered)
}

func TestImportRecoveryRevalidatesPlaceholderDuringDecryption(t *testing.T) {
	for _, mode := range []string{"purged", "reused", "deletion changed"} {
		t.Run(mode, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			f.timeline = []*event.Event{f.message(t, "$original", "original", 10)}
			_, err := f.run(t, false, false)
			req.NoError(err)
			if mode != "deletion changed" {
				req.NoError(f.store.MarkMessageDeleted(f.sourceID, "$original"))
			}
			rooms, err := f.store.MatrixPlaceholderRooms(t.Context(), f.sourceID, nil, nil, attachmentpolicy.Policy{})
			req.NoError(err)
			req.Len(rooms, 1)
			r := backupRuntime(t, f.server.URL, f.backup)
			req.NoError(r.prepareBackup(t.Context()))
			req.NoError(r.fetchRoomBackup(t.Context(), "!room:example.org"))
			key := backupSessionKey{"!room:example.org", f.backup.out.ID()}
			r.cacheSession(key, r.sessionData[key])
			req.NotNil(r.sessions[key])
			r.sessions[key] = ingestDuringDecryption{r.sessions[key], func() error {
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
				if mode == "reused" {
					override := ""
					if f.store.IsPostgreSQL() {
						override = " OVERRIDING SYSTEM VALUE"
					}
					_, err = f.store.DB().Exec(f.store.Rebind(`INSERT INTO messages (id, source_id, conversation_id, source_message_id, message_type, snippet, deleted_from_source_at, deleted_at)`+override+` VALUES (?, ?, ?, '$replacement', 'matrix', ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`), rooms[0].MaxMessageID, f.sourceID, rooms[0].ConversationID, encryptedPlaceholder)
				}
				return err
			}}
			sum := &ImportSummary{PendingEncrypted: 1}
			retired, err := NewImporter(f.store, r).recoverRoom(t.Context(), f.sourceID, rooms[0], 0, ImportOptions{}, sum)
			req.NoError(err)
			check.Zero(retired)
			check.Zero(sum.Recovered)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$original", "$replacement"})
			req.NoError(err)
			if mode == "deletion changed" {
				check.Equal(encryptedPlaceholder, f.body(t, "$original"))
			} else {
				check.Zero(ids["$original"])
			}
			if mode == "reused" {
				check.Equal(rooms[0].MaxMessageID, ids["$replacement"])
			}
		})
	}
}

func TestImportRecoveryRoomFetchFailureContinuesAndPersistsAttempt(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusUnauthorized, http.StatusForbidden} {
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
			last, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
			req.NoError(err)
			state, err := loadSyncState(last.CursorAfter.String)
			req.NoError(err)
			check.False(state.RecoveryRooms["!first:example.org"].TriedAt.IsZero())
		})
	}
}

func TestImportRedactionSkipsUnrelatedPendingEdits(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{
		{ID: "$original", Sender: "@member:example.org", Type: event.EventMessage, Content: event.Content{Parsed: &event.MessageEventContent{MsgType: event.MsgText, Body: "original"}}},
		{ID: "$reaction", Sender: "@member:example.org", Type: event.EventReaction, Content: event.Content{Parsed: &event.ReactionEventContent{RelatesTo: event.RelatesTo{Type: event.RelAnnotation, EventID: "$original", Key: "yes"}}}},
	}
	_, err := f.run(t, false, false)
	req.NoError(err)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$original"})
	req.NoError(err)
	req.NoError(f.store.SetMessageMetadataContext(t.Context(), ids["$original"], sql.NullString{String: `{"matrix_pending_edits":"corrupt"}`, Valid: true}))
	f.timeline = []*event.Event{{ID: "$redaction", Type: event.EventRedaction, Redacts: "$reaction", Content: event.Content{Raw: map[string]any{}}}}
	_, err = f.run(t, false, false)
	req.NoError(err)
	var count int
	req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&count))
	check.Zero(count)
}

func TestImportEditRedactionSkipsUnavailableEdits(t *testing.T) {
	for _, mode := range []string{"missing session", "no key", "transient"} {
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
			_, err = f.run(t, mode != "no key", false)
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
			f.quiet, f.backup.status, f.backup.etag = true, 0, "2"
			_, err = f.run(t, true, false)
			req.NoError(err)
			check.Equal("older", f.body(t, "$original"))
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$original", "$older"})
			req.NoError(err)
			check.Zero(ids["$older"])
			edit, err := NewImporter(f.store, nil).appliedEdit(ids["$original"])
			req.NoError(err)
			check.Equal("$older", edit.EventID)
		})
	}
}

func TestImportEditRedactionRefusesReplayedCiphertext(t *testing.T) {
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
			check.Equal("older", f.body(t, "$original"))
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

func TestImportSkipKeyBackupKeepsEncryptedEditsAndMessages(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	original := f.message(t, "$original", "original", 10)
	older := f.edit(t, "$older", "$original", "older", 20)
	newest := f.edit(t, "$newest", "$original", "newest", 30)
	f.timeline = []*event.Event{original, older, newest}
	_, err := f.run(t, true, false)
	req.NoError(err)
	f.edits = []*event.Event{older}
	f.timeline = []*event.Event{
		{ID: "$redaction", Type: event.EventRedaction, Redacts: "$newest", Content: event.Content{Raw: map[string]any{}}},
		f.message(t, "$pending", "pending", 40),
	}
	versions, rooms := f.backup.versionFetches, f.backup.roomFetches
	r := backupRuntime(t, f.server.URL, f.backup)
	sum, err := NewImporter(f.store, r).Import(t.Context(), ImportOptions{UserID: "@archive:example.org", SkipKeyBackup: true})
	req.NoError(err)
	check.Equal("original", f.body(t, "$original"))
	check.Equal(encryptedPlaceholder, f.body(t, "$pending"))
	check.Equal(versions, f.backup.versionFetches)
	check.Equal(rooms, f.backup.roomFetches)
	check.Zero(sum.Recovered)
	check.NoError(sum.BackupFailure)
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
		if f.historyFails {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeMatrixJSON(t, w, map[string]any{"chunk": f.history, "end": ""})
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

func TestImportRecoversMessagesAndRelations(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	f := newRecoveryFixture(t)
	oldEdit := f.edit(t, "$a-edit", "$z-original", "older", 20)
	newEdit := f.edit(t, "$b-edit", "$z-original", "newest", 30)
	reply := f.backup.encrypted(t, "$reply", 40, event.EventMessage, map[string]any{"msgtype": "m.text", "body": "reply", "m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": "$z-original"}}})
	f.timeline = []*event.Event{oldEdit, newEdit, reply, f.message(t, "$z-original", "original", 10), f.backup.encrypted(t, "$reaction", 50, event.EventReaction, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$z-original", "key": "yes"}}), f.backup.encrypted(t, "$orphan", 60, event.EventReaction, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$absent", "key": "yes"}}), f.backup.encrypted(t, "$unsupported", 70, event.Type{Type: "example.unsupported"}, map[string]any{})}
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
	check.Equal(int64(1), sum.EventsSkipped)
	check.Equal("newest", f.body(t, "$z-original"))
	check.Equal("reply", f.body(t, "$reply"))
	found, err := f.store.MessageExistsBatch(f.sourceID, []string{"$a-edit", "$b-edit", "$reaction", "$orphan", "$unsupported", "$reply", "$z-original"})
	req.NoError(err)
	check.Len(found, 4)
	var hidden bool
	req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT deleted_at IS NOT NULL FROM messages WHERE id = ?`), found["$unsupported"]).Scan(&hidden))
	check.True(hidden)
	sum, err = f.run(t, true, true)
	req.NoError(err)
	check.Zero(sum.PendingEncrypted)
	check.Zero(sum.EventsSkipped)
	var replyTo int64
	req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), found["$reply"]).Scan(&replyTo))
	check.Equal(found["$z-original"], replyTo)
	var reactions int
	req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&reactions))
	check.Equal(1, reactions)
	f.quiet = false
	_, err = f.run(t, false, true)
	req.NoError(err)
	check.Equal("newest", f.body(t, "$z-original"))
	found, err = f.store.MessageExistsBatch(f.sourceID, []string{"$a-edit", "$reaction", "$orphan", "$unsupported"})
	req.NoError(err)
	check.Len(found, 4)
	_, err = f.run(t, true, false)
	req.NoError(err)
	found, err = f.store.MessageExistsBatch(f.sourceID, []string{"$a-edit", "$reaction", "$orphan", "$unsupported"})
	req.NoError(err)
	check.Contains(found, "$unsupported")
	check.Contains(found, "$orphan")
	check.Len(found, 2)
}

func TestImportRecoveryCrossesPlaceholderPages(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{f.edit(t, "$000-edit", "$zzz-original", "updated", 20)}
	for i := range 500 {
		f.timeline = append(f.timeline, f.message(t, fmt.Sprintf("$%03d", i), "filler", 10))
	}
	f.timeline = append(f.timeline, f.message(t, "$zzz-original", "original", 10))
	replay := *f.timeline[1]
	replay.ID = "$zzz-replay"
	f.timeline = append(f.timeline, &replay)
	_, err := f.run(t, false, false)
	req.NoError(err)
	f.quiet = true
	_, err = f.run(t, true, false)
	req.NoError(err)
	check.Equal("updated", f.body(t, "$zzz-original"))
	check.Equal(encryptedPlaceholder, f.body(t, "$zzz-replay"))
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$zzz-original", "$000-edit"})
	req.NoError(err)
	check.Zero(ids["$000-edit"])
	edits, err := NewImporter(f.store, nil).pendingEdits(ids["$zzz-original"])
	req.NoError(err)
	check.Empty(edits)
}

func TestImportRecoversLeftRoomAfterFullSync(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{f.backup.encrypted(t, "$message", 20, event.EventMessage, map[string]any{"msgtype": "m.text", "body": "recovered", "m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": "$older"}}})}
	_, err := f.run(t, false, false)
	req.NoError(err)
	f.quiet, f.left = true, true
	_, err = f.run(t, false, true)
	req.NoError(err)
	sum, err := f.run(t, true, false)
	req.NoError(err)
	check.Equal(int64(1), sum.Recovered)
	check.Equal("recovered", f.body(t, "$message"))
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

func (f *recoveryFixture) ageAttempts(t *testing.T) {
	t.Helper()
	req := require.New(t)
	last, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
	req.NoError(err)
	state, err := loadSyncState(last.CursorAfter.String)
	req.NoError(err)
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

func TestImportRecoveryRetiredRelationsRejectReplay(t *testing.T) {
	for _, kind := range []string{"redacted reaction", "redacted edit", "orphan edit", "orphan reaction"} {
		t.Run(kind, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			first := f.edit(t, "$first", "$original", "edited", 20)
			if strings.HasSuffix(kind, "reaction") {
				first = f.backup.encrypted(t, "$first", 20, event.EventReaction, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$original", "key": "yes"}})
			}
			f.timeline = []*event.Event{first}
			if strings.HasPrefix(kind, "redacted") {
				f.timeline = append(f.timeline, f.message(t, "$original", "original", 10))
			}
			_, err := f.run(t, false, false)
			req.NoError(err)
			if strings.HasPrefix(kind, "redacted") {
				f.timeline = []*event.Event{{ID: "$redaction", Type: event.EventRedaction, Redacts: "$first", Content: event.Content{Raw: map[string]any{}}}}
				_, err = f.run(t, false, false)
				req.NoError(err)
			}
			f.quiet = true
			sum, err := f.run(t, true, false)
			req.NoError(err)
			check.Zero(sum.PendingEncrypted)
			if kind == "redacted reaction" {
				ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$first"})
				req.NoError(err)
				req.NotZero(ids["$first"])
				var hidden bool
				req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT deleted_at IS NOT NULL FROM messages WHERE id = ?`), ids["$first"]).Scan(&hidden))
				check.True(hidden)
			}
			second := *first
			second.ID = "$replayed"
			f.quiet = false
			f.timeline = []*event.Event{&second}
			if strings.HasPrefix(kind, "orphan") {
				f.timeline = append(f.timeline, f.message(t, "$original", "original", 10))
			}
			_, err = f.run(t, true, false)
			req.NoError(err)
			check.Equal("original", f.body(t, "$original"))
			check.Equal(encryptedPlaceholder, f.body(t, "$replayed"))
			var reactions int
			req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&reactions))
			check.Zero(reactions)
		})
	}
}

func TestImportRecoveryFailedRoomsRespectRetryLimit(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.extraRooms = map[string][]*event.Event{}
	f.backup.roomStatuses = map[string]int{}
	const roomCount = recoveryRetryLimit + 3
	for i := range roomCount {
		roomID := fmt.Sprintf("!failed%d:example.org", i)
		evt := f.message(t, "$failed"+strconv.Itoa(i), "failed", 10)
		evt.RoomID = id.RoomID(roomID)
		f.extraRooms[roomID] = []*event.Event{evt}
		f.backup.roomStatuses[roomID] = http.StatusServiceUnavailable
	}
	_, err := f.run(t, false, false)
	req.NoError(err)
	f.quiet = true
	for round := range 3 {
		sum, err := f.run(t, true, false)
		req.ErrorContains(err, "recover Matrix room")
		check.Equal(int64(roomCount), sum.PendingEncrypted)
		check.Equal(roomCount+round*recoveryRetryLimit, f.backup.roomFetches)
		last, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
		req.NoError(err)
		state, err := loadSyncState(last.CursorAfter.String)
		req.NoError(err)
		req.Len(state.RecoveryRooms, roomCount)
		for _, attempt := range state.RecoveryRooms {
			check.False(attempt.TriedAt.IsZero())
			check.Equal(int64(1), attempt.PendingCount)
			check.NotZero(attempt.LastMessageID)
			check.NotEmpty(attempt.LastEventID)
		}
	}
}

func TestImportRecoveryDeletedPlaceholders(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{f.message(t, "$original", "original", 10), f.edit(t, "$edit", "$original", "edited", 20), f.backup.encrypted(t, "$reaction", 30, event.EventReaction, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$original", "key": "yes"}})}
	_, err := f.run(t, false, false)
	req.NoError(err)
	f.timeline = nil
	for _, target := range []string{"$original", "$edit", "$reaction"} {
		f.timeline = append(f.timeline, &event.Event{ID: id.EventID("$redact" + target), Type: event.EventRedaction, Redacts: id.EventID(target), Content: event.Content{Raw: map[string]any{}}})
	}
	_, err = f.run(t, false, false)
	req.NoError(err)
	f.quiet = true
	_, err = f.run(t, true, false)
	req.NoError(err)
	check.Equal("original", f.body(t, "$original"))
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$original", "$edit", "$reaction"})
	req.NoError(err)
	check.Len(ids, 2)
	var deleted bool
	req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), ids["$original"]).Scan(&deleted))
	check.True(deleted)
	var reactions int
	req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&reactions))
	check.Zero(reactions)
}

func TestImportRecoveryInterruptedBeforeAndBetweenWrites(t *testing.T) {
	for _, mode := range []string{"before recovery", "between originals"} {
		t.Run(mode, func(t *testing.T) {
			req := require.New(t)
			check := assert.New(t)
			f := newRecoveryFixture(t)
			original := f.message(t, "$original", "original", 10)
			if mode == "between originals" {
				original = f.backup.encrypted(t, "$original", 10, event.EventMessage, map[string]any{"msgtype": "m.text", "body": "original", "m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": "$later"}}})
			}
			reaction := f.backup.encrypted(t, "$reaction", 20, event.EventReaction, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$original", "key": "yes"}})
			f.timeline = []*event.Event{original, reaction}
			if mode == "between originals" {
				f.timeline = append(f.timeline, f.message(t, "$second", "second", 30))
			}
			if mode == "before recovery" {
				f.historyFails = true
				_, err := f.run(t, true, false)
				req.Error(err)
				check.Equal(encryptedPlaceholder, f.body(t, "$original"))
				f.historyFails = false
			} else {
				_, err := f.run(t, false, false)
				req.NoError(err)
				create := `CREATE TRIGGER stop_original BEFORE UPDATE ON messages WHEN NEW.source_message_id = '$second' BEGIN SELECT RAISE(ABORT, 'interrupted original'); END`
				drop := `DROP TRIGGER stop_original`
				if f.store.IsPostgreSQL() {
					_, err := f.store.DB().Exec(`CREATE FUNCTION stop_original_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.source_message_id = '$second' THEN RAISE EXCEPTION 'interrupted original'; END IF; RETURN NEW; END $$`)
					req.NoError(err)
					create = `CREATE TRIGGER stop_original BEFORE UPDATE ON messages FOR EACH ROW EXECUTE FUNCTION stop_original_fn()`
					drop = `DROP TRIGGER stop_original ON messages`
				}
				_, err = f.store.DB().Exec(create)
				req.NoError(err)
				f.quiet = true
				_, err = f.run(t, true, false)
				req.ErrorContains(err, "interrupted original")
				checkpoint, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
				req.NoError(err)
				state, err := loadSyncState(checkpoint.CursorAfter.String)
				req.NoError(err)
				check.Empty(state.Rooms["!room:example.org"].DeferredRelations, "rolled-back replies must stay out of the cursor")
				check.Equal(encryptedPlaceholder, f.body(t, "$original"))
				check.Equal(encryptedPlaceholder, f.body(t, "$second"))
				check.Equal(encryptedPlaceholder, f.body(t, "$reaction"))
				var reactions int
				req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&reactions))
				check.Zero(reactions)
				_, err = f.store.DB().Exec(drop)
				req.NoError(err)
			}
			f.timeline = nil
			sum, err := f.run(t, true, false)
			req.NoError(err)
			check.Zero(sum.PendingEncrypted)
			check.Equal("original", f.body(t, "$original"))
			var reactions int
			req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&reactions))
			check.Equal(1, reactions)
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

func TestImportRecoverySkipsExcludedRooms(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{f.message(t, "$original", "original", 10)}
	_, err := f.run(t, false, false)
	req.NoError(err)
	f.quiet, f.backup.versionStatus = true, http.StatusServiceUnavailable
	opts := ImportOptions{UserID: "@archive:example.org", ExcludeRooms: []string{"!room:example.org"}}
	sum, err := NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).Import(t.Context(), opts)
	req.NoError(err)
	check.NoError(sum.BackupFailure)
	check.Zero(f.backup.roomFetches)
	check.Zero(f.backup.versionFetches)
}

func TestImportRecoveryResumesBeforePlaceholderCheckpoint(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		t.Run(strconv.FormatBool(quiet), func(t *testing.T) {
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
			req.NoError(NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup)).persistEvent(t.Context(), f.sourceID, convID, fresh, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
			f.timeline, f.quiet = []*event.Event{fresh}, quiet
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
	req := require.New(t)
	check := assert.New(t)
	f := newRecoveryFixture(t)
	f.extraRooms = map[string][]*event.Event{}
	for i := range 12 {
		evt := f.message(t, "$missing"+strconv.Itoa(i), "missing", 10)
		evt.Content.AsEncrypted().SessionID = "absent"
		f.extraRooms[fmt.Sprintf("!missing%d:example.org", i)] = []*event.Event{evt}
	}
	_, err := f.run(t, true, false)
	req.NoError(err)
	check.Equal(12, f.backup.roomFetches)
	f.quiet = true
	_, err = f.run(t, true, false)
	req.NoError(err)
	check.Equal(12, f.backup.roomFetches)
	f.backup.etag = "2"
	_, err = f.run(t, true, false)
	req.NoError(err)
	check.Equal(12, f.backup.roomFetches, "cooldown holds even when the backup changes")
	f.ageAttempts(t)
	_, err = f.run(t, true, false)
	req.NoError(err)
	check.Equal(12+recoveryRetryLimit, f.backup.roomFetches)
	_, err = f.run(t, true, false)
	req.NoError(err)
	check.Equal(12+2*recoveryRetryLimit, f.backup.roomFetches)
	f.quiet = false
	f.timeline = []*event.Event{f.message(t, "$new", "new", 20)}
	_, err = f.run(t, true, false)
	req.NoError(err)
	check.Equal("new", f.body(t, "$new"))
	check.Equal(25, f.backup.roomFetches, "two remaining old rooms and the new room")
	_, err = f.run(t, true, true)
	req.NoError(err)
	check.Equal(37, f.backup.roomFetches, "full sync forces all pending rooms")
}

func TestImportRecoveryProgressDoesNotStarveQuietRoom(t *testing.T) {
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
	last, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
	req.NoError(err)
	state, err := loadSyncState(last.CursorAfter.String)
	req.NoError(err)
	check.False(state.RecoveryRooms[fmt.Sprintf("!busy%d:example.org", recoveryRetryLimit)].TriedAt.Before(began), "the quiet room gets a turn while busy rooms keep recovering events")
	check.Equal("quiet", f.body(t, "$quiet"), "the quiet room recovers once its keys become available")
	check.True(state.RecoveryRooms["!busy0:example.org"].NoProgressAt.IsZero(), "progress clears only the cooldown")
}

func TestImportEditWaitsForEncryptedOriginal(t *testing.T) {
	for _, mode := range []string{"incremental", "full", "redacted plaintext", "redacted encrypted"} {
		t.Run(mode, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			original := f.message(t, "$z-original", "original", 10)
			initialKey := f.backup.data.SessionKey
			if mode == "redacted plaintext" || mode == "redacted encrypted" {
				in, err := olm.NewInboundGroupSession([]byte(f.backup.out.Key()))
				req.NoError(err)
				exported, err := in.Export(1)
				req.NoError(err)
				f.backup.data.SessionKey = string(exported)
			}
			edit := f.edit(t, "$edit", "$z-original", "updated", 20)
			if mode != "redacted encrypted" {
				edit = matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":20,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$z-original"}}}`)
			}
			f.timeline = []*event.Event{original, edit}
			_, err := f.run(t, mode == "redacted plaintext" || mode == "redacted encrypted", false)
			req.NoError(err)
			check.Equal(encryptedPlaceholder, f.body(t, "$z-original"))
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$z-original", "$edit"})
			req.NoError(err)
			if mode == "redacted plaintext" || mode == "redacted encrypted" {
				pending, err := NewImporter(f.store, nil).pendingEdits(ids["$z-original"])
				req.NoError(err)
				req.Len(pending, 1)
				if mode == "redacted encrypted" {
					check.Equal(event.EventMessage, pending[0].Type)
					check.Equal("$edit", pending[0].ID.String())
					req.True(parseContent(pending[0]))
					req.NotNil(pending[0].Content.AsMessage().NewContent)
					check.Equal("updated", pending[0].Content.AsMessage().NewContent.Body)
					f.quiet = true
					sum, err := f.run(t, true, true)
					req.NoError(err)
					check.Equal(int64(1), sum.PendingEncrypted)
					f.quiet = false
				}
				f.timeline = []*event.Event{{ID: "$redaction", Type: event.EventRedaction, Redacts: "$edit", Content: event.Content{Raw: map[string]any{}}}}
				_, err = f.run(t, true, false)
				req.NoError(err)
				pending, err = NewImporter(f.store, nil).pendingEdits(ids["$z-original"])
				req.NoError(err)
				check.Empty(pending)
				f.quiet, f.backup.data.SessionKey = true, initialKey
				_, err = f.run(t, true, true)
				req.NoError(err)
				check.Equal("original", f.body(t, "$z-original"))
				return
			}
			f.quiet = true
			_, err = f.run(t, false, false)
			req.NoError(err)
			last, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
			req.NoError(err)
			check.NotContains(last.CursorAfter.String, "deferred_relations", "edits wait in archive metadata")
			sum, err := f.run(t, true, mode == "full")
			req.NoError(err)
			check.Zero(sum.PendingEncrypted)
			check.Equal("edited", f.body(t, "$z-original"))
			ids, err = f.store.MessageExistsBatch(f.sourceID, []string{"$z-original", "$edit"})
			req.NoError(err)
			check.Zero(ids["$edit"], "edits do not create message rows")
			metadata, err := f.store.GetMessageMetadata(ids["$z-original"])
			req.NoError(err)
			check.NotContains(metadata.String, "matrix_pending_edits")
			_, err = f.run(t, true, false)
			req.NoError(err)
			check.Equal("edited", f.body(t, "$z-original"))
		})
	}
}

func TestImportGCRecoversReusedPlaceholderID(t *testing.T) {
	for _, mode := range []string{"same sync", "later sync", "later sync with new event", "retired relation"} {
		t.Run(mode, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			f := newRecoveryFixture(t)
			missing := f.message(t, "$missing", "missing", 10)
			missing.Content.AsEncrypted().SessionID = "absent"
			retired := mode == "retired relation"
			deleted := f.message(t, "$deleted", "deleted", 20)
			if retired {
				deleted = f.backup.encrypted(t, "$deleted", 20, event.EventReaction, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$missing", "key": "yes"}})
			} else {
				f.backup.status = http.StatusNotFound
			}
			f.timeline = []*event.Event{missing, deleted}
			_, err := f.run(t, !retired, false)
			req.NoError(err)
			last, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
			req.NoError(err)
			before, err := loadSyncState(last.CursorAfter.String)
			req.NoError(err)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$deleted"})
			req.NoError(err)
			deletedID := ids["$deleted"]
			var maxID int64
			req.NoError(f.store.DB().QueryRow(`SELECT MAX(id) FROM messages`).Scan(&maxID))
			req.Equal(maxID, deletedID)
			if retired {
				f.quiet = true
				_, err = f.run(t, true, false)
				req.NoError(err)
				ids, err = f.store.MessageExistsBatch(f.sourceID, []string{"$deleted"})
				req.NoError(err)
				req.Empty(ids)
				f.backup.etag = "2"
				sum, err := f.run(t, true, false)
				req.NoError(err)
				check.Zero(sum.Recovered)
				check.Equal(int64(1), sum.PendingEncrypted)
				check.Equal(2, f.backup.roomFetches, "retiring a relation allows an immediate retry after a backup change")
				f.quiet = false
			} else {
				req.Equal(deletedID, before.RecoveryRooms["!room:example.org"].LastMessageID)
				req.NoError(f.store.MarkMessageDeleted(f.sourceID, "$deleted"))
				if f.store.IsPostgreSQL() {
					// GC is SQLite-only; exercise the equivalent hard delete on PostgreSQL.
					_, err = f.store.DB().Exec(`DELETE FROM messages WHERE id = $1`, deletedID)
					req.NoError(err)
				} else {
					plan, err := f.store.PlanGCContext(t.Context())
					req.NoError(err)
					deleted, err := f.store.ExecuteGCContext(t.Context(), plan)
					req.NoError(err)
					req.Equal(int64(1), deleted)
				}
			}
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
			if !f.store.IsPostgreSQL() {
				check.Equal(deletedID, ids["$new"], "SQLite reuses the collected table-max rowid")
			}
			last, err = f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
			req.NoError(err)
			after, err := loadSyncState(last.CursorAfter.String)
			req.NoError(err)
			if !retired {
				check.Equal(before.RecoveryRooms["!room:example.org"].Stamp, after.RecoveryRooms["!room:example.org"].Stamp)
			}
		})
	}
}

func TestImportUnsupportedEncryptionDoesNotFetchBackupOrStopLaterRoom(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	unsupported := f.message(t, "$unsupported", "unsupported", 10)
	unsupported.Content.AsEncrypted().Algorithm = "example.unsupported"
	f.extraRooms = map[string][]*event.Event{"!first:example.org": {unsupported}}
	f.timeline = []*event.Event{f.message(t, "$later", "later", 20)}
	unsupportedRequests := 0
	handler := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_matrix/client/v3/room_keys/keys/!first:example.org" {
			unsupportedRequests++
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	})
	_, err := f.run(t, false, false)
	req.NoError(err)
	f.quiet = true
	sum, err := f.run(t, true, false)
	req.NoError(err)
	check.Zero(unsupportedRequests)
	req.NoError(sum.BackupFailure)
	check.Equal(int64(1), sum.Recovered)
	check.Equal(int64(1), sum.PendingEncrypted)
	check.Equal("later", f.body(t, "$later"))
}

func TestRecoveryRelationRetirementInterruptedAndResumed(t *testing.T) {
	for _, kind := range []string{"edit", "reaction"} {
		for _, mode := range []string{"interrupted", "resumed"} {
			if kind == "edit" && mode == "resumed" || kind == "reaction" && mode == "interrupted" {
				continue
			}
			t.Run(kind+"/"+mode, func(t *testing.T) {
				resume := mode == "resumed"
				req, check := require.New(t), assert.New(t)
				f := newRecoveryFixture(t)
				original := f.message(t, "$original", "original", 10)
				f.timeline = []*event.Event{original}
				_, err := f.run(t, true, false)
				req.NoError(err)
				rel := f.edit(t, "$relation", "$original", "edited", 20)
				if kind == "reaction" {
					rel = f.backup.encrypted(t, "$relation", 20, event.EventReaction, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$original", "key": "yes"}})
				}
				f.timeline = []*event.Event{rel}
				if resume {
					f.timeline = append(f.timeline, f.backup.encrypted(t, "$second-reaction", 30, event.EventReaction, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$original", "key": "second"}}))
				}
				_, err = f.run(t, false, false)
				req.NoError(err)
				stopID := "$relation"
				if resume {
					stopID = "$second-reaction"
				}
				create := fmt.Sprintf(`CREATE TRIGGER stop_retirement BEFORE DELETE ON messages WHEN OLD.source_message_id = '%s' BEGIN SELECT RAISE(ABORT, 'interrupted retirement'); END`, stopID)
				drop := `DROP TRIGGER stop_retirement`
				if f.store.IsPostgreSQL() {
					_, err := f.store.DB().Exec(fmt.Sprintf(`CREATE FUNCTION stop_retirement_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF OLD.source_message_id = '%s' THEN RAISE EXCEPTION 'interrupted retirement'; END IF; RETURN OLD; END $$`, stopID))
					req.NoError(err)
					create = `CREATE TRIGGER stop_retirement BEFORE DELETE ON messages FOR EACH ROW EXECUTE FUNCTION stop_retirement_fn()`
					drop = `DROP TRIGGER stop_retirement ON messages`
				}
				_, err = f.store.DB().Exec(create)
				req.NoError(err)
				f.quiet = true
				_, err = f.run(t, true, false)
				req.ErrorContains(err, "interrupted retirement")
				check.Equal("original", f.body(t, "$original"))
				check.Equal(encryptedPlaceholder, f.body(t, "$relation"))
				if resume {
					check.Equal(encryptedPlaceholder, f.body(t, "$second-reaction"))
				}
				var reactions int
				req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&reactions))
				check.Zero(reactions)
				_, err = f.store.DB().Exec(drop)
				req.NoError(err)
				if resume {
					sum, err := f.run(t, true, false)
					req.NoError(err)
					check.Zero(sum.PendingEncrypted)
					check.Equal("original", f.body(t, "$original"))
					req.NoError(f.store.DB().QueryRow(`SELECT count(*) FROM reactions`).Scan(&reactions))
					check.Equal(2, reactions)
				}
			})
		}
	}
}

func TestImportRecoveryCountsSameRunOnce(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{f.message(t, "$original", "original", 10)}
	sum, err := f.run(t, true, false)
	req.NoError(err)
	check.Equal(int64(1), sum.MessagesProcessed)
	check.Equal(int64(1), sum.MessagesAdded)
	check.Equal(int64(1), sum.Recovered)
	check.Zero(sum.PendingEncrypted)
	sync, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
	req.NoError(err)
	check.Equal(int64(1), sync.MessagesProcessed)
}

func TestImportNewPlaceholderKeepsOldRecoveryCooldown(t *testing.T) {
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
}

func TestImportInterruptedNewPlaceholdersPreserveRecoveryCheckpoint(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.extraRooms = map[string][]*event.Event{}
	for i := range 7 {
		missing := f.message(t, fmt.Sprintf("$old%d", i), "old", 10)
		missing.Content.AsEncrypted().SessionID = "absent"
		f.extraRooms[fmt.Sprintf("!missing%d:example.org", i)] = []*event.Event{missing}
	}
	_, err := f.run(t, true, false)
	req.NoError(err)
	last, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
	req.NoError(err)
	before, err := loadSyncState(last.CursorAfter.String)
	req.NoError(err)
	for i := range 7 {
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
}

func TestImportFailingRecoveryRoomDoesNotStarveLaterRooms(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{f.message(t, "$poison", "poison", 10)}
	_, err := f.run(t, false, false)
	req.NoError(err)
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$poison"})
	req.NoError(err)
	req.NoError(f.store.SetMessageMetadataContext(t.Context(), ids["$poison"], sql.NullString{String: `{"matrix_event_type":"m.room.encrypted","matrix_pending_edits":"corrupt"}`, Valid: true}))
	later := f.message(t, "$later", "later", 20)
	cipher, err := f.backup.out.Encrypt([]byte(`{"room_id":"!later:example.org","type":"m.room.message","content":{"msgtype":"m.text","body":"later"}}`))
	req.NoError(err)
	later.Content.AsEncrypted().MegolmCiphertext = cipher
	f.extraRooms = map[string][]*event.Event{"!later:example.org": {later}}
	for round := range 2 {
		f.nextBatch = fmt.Sprintf("after%d", round)
		sum, err := f.run(t, true, false)
		req.ErrorContains(err, "recover Matrix room !room:example.org")
		check.Equal(int64(1), sum.PendingEncrypted)
		check.Equal(encryptedPlaceholder, f.body(t, "$poison"))
		run := f.operation(t)
		check.Equal(operations.StatePartial, run.State)
		check.Contains(run.Counters, operations.PublicCounter{Name: operations.CounterItemErrors, Unit: operations.CounterUnitMessages, Value: 1})
		last, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
		req.NoError(err)
		state, err := loadSyncState(last.CursorAfter.String)
		req.NoError(err)
		check.Equal(f.nextBatch, state.NextBatch)
		check.False(state.RecoveryRooms["!room:example.org"].TriedAt.IsZero())
		if round == 0 {
			check.Equal(int64(1), sum.Recovered)
			check.Equal("later", f.body(t, "$later"))
			f.quiet = true
		}
	}
}

func TestImportRecoveryPreservesUnconvertedCiphertext(t *testing.T) {
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
	check.Len(items, 1)
	for _, item := range items {
		check.Contains(string(item.Raw), `"ciphertext"`)
	}
}

func TestImportCorruptBackupKeyAllowsPlaintextSync(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{{ID: "$plain", Sender: "@member:example.org", Type: event.EventMessage, Content: event.Content{Parsed: &event.MessageEventContent{MsgType: event.MsgText, Body: "plain"}}}}
	r, err := Open(Credentials{Homeserver: f.server.URL, UserID: "@archive:example.org", AccessToken: "token", BackupKey: "corrupt!"})
	req.NoError(err)
	sum, err := NewImporter(f.store, r).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	req.NoError(err)
	check.NoError(sum.BackupFailure)
	check.Equal("plain", f.body(t, "$plain"))
	f.timeline = []*event.Event{f.message(t, "$pending", "pending", 10)}
	sum, err = NewImporter(f.store, r).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	req.NoError(err)
	req.ErrorContains(sum.BackupFailure, "decode Matrix backup key")
	check.Equal(int64(1), sum.PendingEncrypted)
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
	_, err = loadSyncState(`{"format_version":1,"media_discovery_version":2,"rooms":{}}`)
	check.ErrorIs(err, errCursorVersion)
}

func TestImportRecoveredReplyLinksWhenTargetArrivesLater(t *testing.T) {
	for _, mode := range []string{"incremental", "full", "edited reply", "interrupted after page commit", "source deleted"} {
		t.Run(mode, func(t *testing.T) {
			req := require.New(t)
			check := assert.New(t)
			f := newRecoveryFixture(t)
			reply := f.backup.encrypted(t, "$reply", 20, event.EventMessage, map[string]any{"msgtype": "m.text", "body": "reply", "m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": "$target"}}})
			f.timeline = []*event.Event{reply}
			if mode == "source deleted" {
				_, err := f.run(t, false, false)
				req.NoError(err)
				req.NoError(f.store.MarkMessageDeleted(f.sourceID, "$reply"))
				ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$reply"})
				req.NoError(err)
				req.NoError(f.store.SetMessageMetadataContext(t.Context(), ids["$reply"], sql.NullString{String: `{"saved":"keep"}`, Valid: true}))
				f.quiet = true
			}
			full := strings.HasSuffix(mode, "full")
			sum, err := f.run(t, mode != "interrupted after page commit" && mode != "edited reply", false)
			req.NoError(err)
			if mode == "interrupted after page commit" {
				imp := NewImporter(f.store, backupRuntime(t, f.server.URL, f.backup))
				req.NoError(imp.runtime.prepareBackup(t.Context()))
				rooms, err := f.store.MatrixPlaceholderRooms(t.Context(), f.sourceID, nil, nil, attachmentpolicy.Policy{})
				req.NoError(err)
				req.Len(rooms, 1)
				// Discard the recovered cursor to simulate exit before checkpoint persistence.
				_, err = imp.recoverRoom(t.Context(), f.sourceID, rooms[0], 0, ImportOptions{}, sum)
				req.NoError(err)
			}
			wantBody := "reply"
			if mode == "edited reply" {
				f.quiet = true
				_, err = f.run(t, true, false)
				req.NoError(err)
				check.Equal("reply", f.body(t, "$reply"))
				f.quiet = false
				f.timeline = []*event.Event{f.edit(t, "$reply-edit", "$reply", "edited reply", 40)}
				_, err = f.run(t, true, false)
				req.NoError(err)
				check.Equal("edited reply", f.body(t, "$reply"))
				f.quiet = true
				sum, err = f.run(t, true, true)
				req.NoError(err)
				check.Equal("edited reply", f.body(t, "$reply"))
				wantBody = "edited reply"
			}
			check.Zero(sum.PendingEncrypted)
			check.Equal(wantBody, f.body(t, "$reply"))
			replies, err := f.store.ListUnresolvedMessageRepliesAfterContext(t.Context(), f.sourceID, SourceType, 0, pageSize, 0)
			req.NoError(err)
			req.Len(replies, 1)
			check.Contains(replies[0].Metadata, `"referenced_message_id"`)
			saved, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
			req.NoError(err)
			check.NotContains(saved.CursorAfter.String, "deferred_relations")
			if full {
				syncID, err := f.store.StartSync(f.sourceID, SourceType)
				req.NoError(err)
				req.NoError(f.store.CompleteSync(syncID, "invalid JSON"))
			}
			f.timeline = nil
			_, err = f.run(t, true, full)
			req.NoError(err)
			f.quiet = false
			f.timeline = []*event.Event{f.message(t, "$target", "target", 10)}
			_, err = f.run(t, true, full)
			req.NoError(err)
			ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$reply", "$target"})
			req.NoError(err)
			var targetID int64
			req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), ids["$reply"]).Scan(&targetID))
			check.Equal(ids["$target"], targetID)
			if mode == "source deleted" {
				var deleted bool
				req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), ids["$reply"]).Scan(&deleted))
				check.True(deleted)
				metadata, err := f.store.GetMessageMetadata(ids["$reply"])
				req.NoError(err)
				var fields map[string]any
				req.NoError(json.Unmarshal([]byte(metadata.String), &fields))
				check.Equal(map[string]any{"matrix_event_type": "m.room.encrypted", "referenced_message_id": "$target", "saved": "keep", fmt.Sprintf("matrix_megolm_%s_0", f.backup.out.ID()): "$reply"}, fields)
			}
			check.Equal(wantBody, f.body(t, "$reply"))
		})
	}
}

func TestImportPendingRepliesCrossPagesWithoutRawEvents(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	convID, err := f.store.EnsureConversation(f.sourceID, "!room:example.org", "Room")
	req.NoError(err)
	otherConvID, err := f.store.EnsureConversation(f.sourceID, "!other:example.org", "Other")
	req.NoError(err)
	var replyIDs []int64
	for i := range pageSize + 2 {
		room := convID
		if i == 0 {
			room = otherConvID
		}
		messageID, err := f.store.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: f.sourceID, ConversationID: room, SourceMessageID: fmt.Sprintf("$reply-%d", i), MessageType: SourceType}})
		req.NoError(err)
		req.NoError(f.store.SetMessageMetadataContext(t.Context(), messageID, sql.NullString{String: `{"referenced_message_id":"$target"}`, Valid: true}))
		replyIDs = append(replyIDs, messageID)
	}
	f.timeline = []*event.Event{f.message(t, "$target", "target", 10)}
	_, err = f.run(t, false, false)
	req.NoError(err)
	targets, err := f.store.MessageExistsBatch(f.sourceID, []string{"$target"})
	req.NoError(err)
	for i, messageID := range replyIDs {
		var targetID sql.NullInt64
		req.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), messageID).Scan(&targetID))
		if i == 0 {
			check.False(targetID.Valid)
		} else {
			check.Equal(targets["$target"], targetID.Int64)
		}
	}
}

func TestImportNewPlaceholderRetriesAfterBackupFailure(t *testing.T) {
	for _, failure := range []string{"version", "room"} {
		t.Run(failure, func(t *testing.T) {
			req := require.New(t)
			check := assert.New(t)
			f := newRecoveryFixture(t)
			f.timeline = []*event.Event{f.message(t, "$first", "first", 10)}
			sum, err := f.run(t, true, false)
			req.NoError(err)
			check.Equal(int64(1), sum.Recovered)
			f.timeline = []*event.Event{f.message(t, "$new", "new", 20)}
			if failure == "version" {
				f.backup.versionStatus = 503
			} else {
				f.backup.status = 503
			}
			sum, err = f.run(t, true, false)
			if failure == "room" {
				req.ErrorContains(err, "fetch Matrix room backup")
				req.NoError(sum.BackupFailure)
			} else {
				req.NoError(err)
				req.Error(sum.BackupFailure)
			}
			run := f.operation(t)
			check.Equal(operations.StatePartial, run.State)
			check.Contains(run.Counters, operations.PublicCounter{Name: operations.CounterItemErrors, Unit: operations.CounterUnitMessages, Value: 1})
			check.Contains(run.Counters, operations.PublicCounter{Name: operations.CounterAdded, Unit: operations.CounterUnitMessages, Value: 1})
			check.Equal(int64(1), sum.PendingEncrypted)
			check.Equal(encryptedPlaceholder, f.body(t, "$new"))
			f.quiet, f.backup.status, f.backup.versionStatus = true, 0, 0
			sum, err = f.run(t, true, false)
			req.NoError(err)
			check.Equal(int64(1), sum.Recovered)
			check.Zero(sum.PendingEncrypted)
			check.Equal("new", f.body(t, "$new"))
		})
	}
}

func TestImportKeylessPendingCountIncludesEarlierRuns(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{f.message(t, "$first", "first", 10)}
	sum, err := f.run(t, false, false)
	req.NoError(err)
	check.Equal(int64(1), sum.PendingEncrypted)
	f.timeline = []*event.Event{f.message(t, "$second", "second", 20)}
	sum, err = f.run(t, false, false)
	req.NoError(err)
	check.Equal(int64(2), sum.PendingEncrypted)
	f.quiet = true
	sum, err = f.run(t, false, true)
	req.NoError(err)
	check.Equal(int64(2), sum.PendingEncrypted)
}

func TestImportRefusesUnreadableCursor(t *testing.T) {
	for _, full := range []bool{false, true} {
		for _, blob := range []string{`{"format_version":2,"rooms":{}}`, `{"rooms":false}`, `invalid JSON`} {
			t.Run(fmt.Sprintf("full=%t/%s", full, blob), func(t *testing.T) {
				req, check := require.New(t), assert.New(t)
				f := newRecoveryFixture(t)
				syncID, err := f.store.StartSync(f.sourceID, SourceType)
				req.NoError(err)
				req.NoError(f.store.CompleteSync(syncID, blob))
				handler := f.server.Config.Handler
				f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if full && r.URL.Path == "/_matrix/client/v3/sync" {
						check.Empty(r.URL.Query().Get("since"))
					}
					handler.ServeHTTP(w, r)
				})
				_, err = f.run(t, true, full)
				if full && blob != `{"format_version":2,"rooms":{}}` {
					req.NoError(err)
					latest, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
					req.NoError(err)
					check.NotEqual(syncID, latest.ID)
					return
				}
				req.Error(err)
				latest, err := f.store.GetLastSuccessfulSyncByType(f.sourceID, SourceType)
				req.NoError(err)
				check.Equal(syncID, latest.ID)
				check.Equal(blob, latest.CursorAfter.String)
				check.Zero(f.backup.versionFetches)
			})
		}
	}
}

func TestImportPlaceholderTextDoesNotRequestBackup(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	f := newRecoveryFixture(t)
	f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$plaintext","sender":"@member:example.org","content":{"msgtype":"m.text","body":"[encrypted message — keys unavailable]"}}`)}
	sum, err := f.run(t, true, false)
	req.NoError(err)
	check.Zero(sum.PendingEncrypted)
	check.Zero(f.backup.versionFetches)
	check.Zero(f.backup.roomFetches)
	check.Equal(encryptedPlaceholder, f.body(t, "$plaintext"))
	f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$ordinary","sender":"@member:example.org","origin_server_ts":10,"content":{"msgtype":"m.text","body":"ordinary"}}`)}
	_, err = f.run(t, true, false)
	req.NoError(err)
	f.backup.versionStatus = http.StatusServiceUnavailable
	f.timeline = []*event.Event{matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":20,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"[encrypted message — keys unavailable]"},"m.relates_to":{"rel_type":"m.replace","event_id":"$ordinary"}}}`)}
	sum, err = f.run(t, true, false)
	req.NoError(err)
	check.Zero(sum.PendingEncrypted)
	req.NoError(sum.BackupFailure)
	check.Zero(f.backup.versionFetches)
	check.Equal(encryptedPlaceholder, f.body(t, "$ordinary"))
	f.quiet = true
	sum, err = f.run(t, true, false)
	req.NoError(err)
	check.Zero(sum.PendingEncrypted)
	req.NoError(sum.BackupFailure)
	check.Zero(f.backup.versionFetches)
}

func TestImportRecoveryRejectsReplayedIndex(t *testing.T) {
	for _, kind := range []string{"message", "edit", "reaction"} {
		for _, later := range []bool{false, true} {
			t.Run(kind+"/later="+strconv.FormatBool(later), func(t *testing.T) {
				req, check := require.New(t), assert.New(t)
				f := newRecoveryFixture(t)
				if kind != "message" {
					f.timeline = []*event.Event{f.message(t, "$original", "original", 1)}
					_, err := f.run(t, true, false)
					req.NoError(err)
				}
				first := f.message(t, "$first", "recovered", 10)
				switch kind {
				case "edit":
					first = f.edit(t, "$first", "$original", "edited", 10)
				case "reaction":
					first = f.backup.encrypted(t, "$first", 10, event.EventReaction, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$original", "key": "yes"}})
				}
				second := *first
				second.ID, second.Sender = "$second", "@forged:example.org"
				f.timeline = []*event.Event{first}
				if !later {
					f.timeline = append(f.timeline, &second)
				}
				_, err := f.run(t, true, false)
				req.NoError(err)
				if later {
					f.timeline = []*event.Event{&second}
					_, err = f.run(t, true, false)
					req.NoError(err)
				}
				check.Equal(encryptedPlaceholder, f.body(t, "$second"))
				ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$second"})
				req.NoError(err)
				raw, err := f.store.GetMessageRaw(ids["$second"])
				req.NoError(err)
				check.Contains(string(raw), `"ciphertext"`)
				metadata, err := f.store.GetMessageMetadata(ids["$second"])
				req.NoError(err)
				check.JSONEq(`{"matrix_event_type":"m.room.encrypted"}`, metadata.String)
				var recovered int
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
			f.quiet, f.left, f.historyFails = true, true, true
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
			refs, err := f.store.MessageMatrixAttachments(ids["$photo"])
			req.NoError(err)
			ref := refs["matrix:mxc://example.org/photo"]
			check.Equal(attachmentpolicy.StateStored, ref.State)
			bytes, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, ref.StoragePath))
			req.NoError(err)
			check.Equal("photo bytes", string(bytes))
		})
	}
}
