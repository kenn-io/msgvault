package recordingref

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func recordingBody(t *testing.T, f *storetest.Fixture, id int64, body string) {
	t.Helper()
	require.NoError(t, f.Store.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
}

func recordingState(t *testing.T, f *storetest.Fixture, id int64) (state, code, operation string) {
	t.Helper()
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`SELECT state,error_code,operation_id FROM recording_references WHERE message_id=? LIMIT 1`), id).Scan(&state, &code, &operation))
	return
}

func due(t *testing.T, f *storetest.Fixture) {
	t.Helper()
	_, err := f.Store.DB().Exec(`UPDATE recording_references SET next_action_at='2000-01-01 00:00:00.000'`)
	require.NoError(t, err)
}

func runDiscovery(t *testing.T, w *Worker, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		err := w.RunBatch(ctx)
		require.NoError(t, err)
		var got int
		require.NoError(t, w.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM recording_references`).Scan(&got))
		if got == count {
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			require.NoError(t, ctx.Err(), "discovered %d recording references, want %d", got, count)
		}
	}
}

func waitForRecordingChange(t *testing.T, w *Worker, id int64) {
	t.Helper()
	require := require.New(t)
	before, err := w.st.LoadRecordingReferenceCursor(t.Context(), w.destination)
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		changed, err := w.st.ListChangedMessages(ctx, before.FeedCursor(), 200)
		require.NoError(err)
		require.NoError(w.RunBatch(ctx))
		after, err := w.st.LoadRecordingReferenceCursor(ctx, w.destination)
		require.NoError(err)
		for _, message := range changed.Messages {
			if message.ID == id && (after.At.After(message.ContentChangedAt) || after.At.Equal(message.ContentChangedAt) && (!after.AfterRow || after.AfterID >= id)) {
				return
			}
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			require.NoError(ctx.Err(), "message %d must reach the recording reference cursor", id)
		}
	}
}

func recordingClient(t *testing.T, handler http.HandlerFunc) *docbankmedia.Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	c, err := docbankmedia.NewClient(s.URL, nil)
	require.NoError(t, err)
	return c
}

func recordingURLClient(t *testing.T, requests chan<- string) *docbankmedia.Client {
	t.Helper()
	return recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req docbankmedia.ReferenceRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &req)) {
			return
		}
		requests <- req.ReferenceURL
		writeReceipt(t, w, req.OperationID)
	})
}

func writeReceipt(t *testing.T, w http.ResponseWriter, op string) {
	t.Helper()
	assert.NoError(t, json.MarshalWrite(w, docbankmedia.Receipt{VaultUID: "vault", SourceID: "source", OperationID: op, OccurrenceID: "occurrence", OperationState: "succeeded", CoverageState: "unprocessed", Outcome: "access_required"}))
}

func TestRecordingReferenceFeed(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	requests := make(chan string, 4)
	client := recordingURLClient(t, requests)
	one := f.CreateMessage("one")
	recordingBody(t, f, one, "https://loom.com/share/one")
	w := NewWorker(f.Store, client, "destination", nil, nil)
	runDiscovery(t, w, 1)
	_, _, operation := recordingState(t, f, one)
	skipped := f.CreateMessage("skipped")
	recordingBody(t, f, skipped, `[https://loom.com/share/label](https://loom.com/share/skipped "title")`)
	before, err := f.Store.LoadRecordingReferenceCursor(t.Context(), "destination")
	require.NoError(err)
	oldPolicy := digest("discovery-v5\x00")
	_, err = f.Store.AdvanceRecordingReferenceCursor(t.Context(), "destination", before, store.RecordingReferenceCursor{At: time.Now().Add(time.Hour), Policy: oldPolicy})
	require.NoError(err)
	runDiscovery(t, w, 2)
	require.Len(requests, 2)
	<-requests
	assert.Equal("https://loom.com/share/skipped", <-requests)
	state, _, after := recordingState(t, f, one)
	assert.Equal("retained", state)
	assert.Equal(operation, after)
	cursor, err := f.Store.LoadRecordingReferenceCursor(t.Context(), "destination")
	require.NoError(err)
	assert.NotEqual(oldPolicy, cursor.Policy)
	source, err := f.Store.GetOrCreateSource("slack", "alice")
	require.NoError(err)
	conv, err := f.Store.EnsureConversation(source.ID, "thread", "Thread")
	require.NoError(err)
	two, err := f.Store.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "two", MessageType: "slack"})
	require.NoError(err)
	recordingBody(t, f, two, "https://cap.example.test/s/two")
	waitForRecordingChange(t, w, two)
	var count int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM recording_references WHERE message_id=?`), two).Scan(&count))
	assert.Zero(count)
	w = NewWorker(f.Store, client, "destination", []string{"https://cap.example.test"}, nil)
	runDiscovery(t, w, 3)
	recordingBody(t, f, one, "recording removed")
	waitForRecordingChange(t, w, one)
	state, _, _ = recordingState(t, f, one)
	assert.Equal("withdrawn", state)
}

func TestRecordingReferenceLegacyBeeper(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE sources SET source_type='beeper' WHERE id=?`), f.Source.ID)
	require.NoError(err)
	id := f.CreateMessage("recording")
	recordingBody(t, f, id, "Watch recording")
	require.NoError(f.Store.UpsertMessageRawWithFormat(id, []byte(`{"text":"<a href=\"https://loom.com/share/abc!\">Watch recording</a>"}`), "beeper_json"))
	requests := make(chan string, 2)
	client := recordingURLClient(t, requests)
	w := NewWorker(f.Store, client, "destination", nil, nil)
	runDiscovery(t, w, 1)
	require.Len(requests, 1)
	assert.Equal("https://loom.com/share/abc!", <-requests)
	before, err := f.Store.LoadRecordingReferenceCursor(t.Context(), "destination")
	require.NoError(err)
	_, err = f.Store.AdvanceRecordingReferenceCursor(t.Context(), "destination", before, store.RecordingReferenceCursor{Policy: before.Policy})
	require.NoError(err)
	before, err = f.Store.LoadRecordingReferenceCursor(t.Context(), "destination")
	require.NoError(err)
	_, err = f.Store.DB().Exec(`ALTER TABLE message_raw RENAME TO recording_raw_unavailable`)
	require.NoError(err)
	require.Error(w.RunBatch(t.Context()))
	after, err := f.Store.LoadRecordingReferenceCursor(t.Context(), "destination")
	require.NoError(err)
	assert.Equal(before, after)
	_, err = f.Store.DB().Exec(`ALTER TABLE recording_raw_unavailable RENAME TO message_raw`)
	require.NoError(err)
	for _, raw := range [][]byte{[]byte(`{"text":`), nil} {
		if raw == nil {
			_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM message_raw WHERE message_id=?`), id)
			require.NoError(err)
		} else {
			require.NoError(f.Store.UpsertMessageRawWithFormat(id, raw, "beeper_json"))
		}
		m, exists, err := w.readMessage(t.Context(), id)
		require.NoError(err)
		require.True(exists)
		assert.Equal("Watch recording", m.Body)
		assert.Empty(messageRefs(m, nil))
	}
}

func TestRecordingReferenceHTMLVisibleURL(t *testing.T) {
	for _, tc := range []struct {
		name, href, before, after, label string
		extraAnchors                     int
		repeated                         bool
	}{
		{"suffix with anchors", "https://cap.so/s/abc!", "", "...", "", 8, false},
		{"encoded repeated anchor", "https://loom.com/share/abc?token=one&key=two!…", "", "", "", 0, true},
		{"different URL label", "https://cap.so/s/target", "", "", "https://loom.com/share/label", 0, false},
	} {
		for _, textVariant := range []string{"both", "plain text without URL"} {
			t.Run(tc.name+"/"+textVariant, func(t *testing.T) {
				assert := assert.New(t)
				label := tc.label
				if label == "" {
					label = tc.href
				}
				visible := tc.before + label + tc.after
				text := visible + " https://cap.so/s/other"
				anchor := `<a href="` + strings.ReplaceAll(tc.href, "&", "&amp;") + `">` + label + `</a>`
				body := tc.before + anchor + tc.after + " https://cap.so/s/other"
				if tc.repeated {
					body += " " + anchor
				}
				want := []string{tc.href, "https://cap.so/s/other"}
				var anchors strings.Builder
				for i := range tc.extraAnchors {
					href := "https://cap.so/s/extra" + strconv.Itoa(i)
					anchors.WriteString(` <a href="` + href + `">recording</a>`)
					want = append(want, href)
				}
				body += anchors.String()
				text += strings.Repeat(" recording", tc.extraAnchors)
				if textVariant == "plain text without URL" {
					text = "Watch recording"
				}
				m := store.RecordingMessage{Body: text, BodyHTML: body}
				refs := messageRefs(m, nil)
				var got []string
				for _, ref := range refs {
					got = append(got, ref.Reference)
				}
				assert.ElementsMatch(want, got)
			})
		}
	}
}

func TestRecordingReferenceTeamsPointer(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("teams")
	require.NoError(t, f.Store.ReplaceMessageLinkAttachments(id, []store.AttachmentRef{{SourceAttachmentID: "teams:recording:abc", StoragePath: "https://teams.example.test/play?token=secret"}}))
	requests := make(chan string, 1)
	client := recordingURLClient(t, requests)
	runDiscovery(t, NewWorker(f.Store, client, "destination", nil, nil), 1)
	require.Len(t, requests, 1)
	assert.Equal(t, "https://teams.example.test/play?token=secret", <-requests)
}

func TestRecordingReferenceDelivery(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		code, state string
	}{
		{"retry", 503, "server_error", "uncertain"},
		{"blocked", 422, "validation", "blocked"},
		{"capability", 503, "capability_unavailable", "uncertain"},
		{"credential", 200, "credential_unavailable", "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := storetest.New(t)
			id := f.CreateMessage("recording")
			recordingBody(t, f, id, "https://loom.com/share/abc?token=one")
			var requests []docbankmedia.ReferenceRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req docbankmedia.ReferenceRequest
				if !assert.NoError(json.UnmarshalRead(r.Body, &req)) {
					return
				}
				requests = append(requests, req)
				if tc.status != 200 {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"code":"` + tc.code + `"}`))
					return
				}
				writeReceipt(t, w, req.OperationID)
			}))
			defer server.Close()
			client, err := docbankmedia.NewClient(server.URL, func() (string, error) {
				if tc.name == "credential" {
					return "", docbankmedia.ErrCredentialUnavailable
				}
				return "", nil
			})
			require.NoError(err)
			w := NewWorker(f.Store, client, "destination", nil, nil)
			runDiscovery(t, w, 1)
			state, code, _ := recordingState(t, f, id)
			assert.Equal(tc.state, state)
			assert.Equal(tc.code, code)
			if tc.name == "credential" {
				assert.Empty(requests)
			}
			if tc.name == "capability" {
				due(t, f)
				err := w.RunBatch(t.Context())
				require.NoError(err)
				claims, err := f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
				require.NoError(err)
				require.Len(claims, 1)
				assert.Equal(2, claims[0].RetryCount)
				assert.WithinDuration(time.Now().Add(10*time.Minute), claims[0].NextActionAt, 5*time.Second)
			}
		})
	}
}

func TestRecordingReferenceUncertain(t *testing.T) {
	for _, change := range []string{"hide", "replace"} {
		t.Run("canceled completion then "+change, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := storetest.New(t)
			id := f.CreateMessage("recording")
			recordingBody(t, f, id, "https://loom.com/share/abc?token=old")
			m, exists, err := f.Store.ReadRecordingMessage(t.Context(), id)
			require.NoError(err)
			require.True(exists)
			inputs, err := referenceInputs(m, messageRefs(m, nil))
			require.NoError(err)
			require.NoError(f.Store.ReconcileRecordingReferences(t.Context(), "destination", id, true, inputs))
			claims, err := f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
			require.NoError(err)
			require.Len(claims, 1)
			oldOperation := claims[0].OperationID
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			gets := make(chan string, 5)
			client := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					gets <- r.URL.Path
					writeReceipt(t, w, oldOperation)
					return
				}
				var req docbankmedia.ReferenceRequest
				if !assert.NoError(json.UnmarshalRead(r.Body, &req)) {
					return
				}
				var state, op string
				if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT state,operation_id FROM recording_references WHERE message_id=?`), id).Scan(&state, &op)) {
					return
				}
				assert.Equal("uncertain", state)
				assert.Equal(oldOperation, op)
				var sent sql.NullTime
				if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT last_send_at FROM recording_references WHERE message_id=?`), id).Scan(&sent)) {
					return
				}
				assert.True(sent.Valid)
				cancel()
				writeReceipt(t, w, req.OperationID)
			})
			var logs bytes.Buffer
			worker := NewWorker(f.Store, client, "destination", nil, slog.New(slog.NewTextHandler(&logs, nil)))
			require.Error(worker.deliver(ctx, claims[0]))
			state, _, op := recordingState(t, f, id)
			assert.Equal("uncertain", state)
			assert.Equal(oldOperation, op)
			if change == "hide" {
				_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=? WHERE id=?`), "2000-01-01 00:00:00.000", id)
				require.NoError(err)
			} else {
				recordingBody(t, f, id, "https://loom.com/share/abc?token=new")
			}
			due(t, f)
			err = worker.RunBatch(t.Context())
			require.NoError(err)
			require.Len(gets, 1)
			assert.Equal("/api/v1/media/operations/"+oldOperation, <-gets)
			assert.Contains(logs.String(), "source_id=source")
			assert.Contains(logs.String(), "occurrence_id=occurrence")
			state, _, op = recordingState(t, f, id)
			if change == "hide" {
				assert.Equal("withdrawn", state)
				assert.Equal(oldOperation, op)
			} else {
				assert.Equal("pending", state)
				assert.NotEqual(oldOperation, op)
			}
		})
	}
	for _, code := range []string{"unauthorized", "server_error", "credential_unavailable", "receipt_server_error"} {
		t.Run("failed recovery/"+code, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := storetest.New(t)
			id := f.CreateMessage("recording")
			recordingBody(t, f, id, "https://loom.com/share/abc?token=old")
			t.Setenv("MSGVAULT_REFERENCE_TEST_KEY", "old-key")
			var expectedKey, oldOperation atomic.Value
			expectedKey.Store("old-key")
			var lost, fail, missingKey atomic.Bool
			gets := make(chan string, 3)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					gets <- r.URL.Path
				}
				expected, ok := expectedKey.Load().(string)
				if !assert.True(ok) {
					return
				}
				if r.Header.Get("X-Api-Key") != expected {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if fail.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if r.Method == http.MethodGet {
					writeReceipt(t, w, strings.TrimPrefix(r.URL.Path, "/api/v1/media/operations/"))
					return
				}
				var req docbankmedia.ReferenceRequest
				if !assert.NoError(json.UnmarshalRead(r.Body, &req)) {
					return
				}
				if lost.CompareAndSwap(false, true) {
					oldOperation.Store(req.OperationID)
					hijacker, ok := w.(http.Hijacker)
					if !assert.True(ok) {
						return
					}
					conn, _, err := hijacker.Hijack()
					if !assert.NoError(err) {
						return
					}
					_ = conn.Close()
					return
				}
				writeReceipt(t, w, req.OperationID)
			}))
			defer server.Close()
			client, err := docbankmedia.NewClient(server.URL, func() (string, error) {
				if missingKey.Load() {
					return "", docbankmedia.ErrCredentialUnavailable
				}
				return os.Getenv("MSGVAULT_REFERENCE_TEST_KEY"), nil
			})
			require.NoError(err)
			worker := NewWorker(f.Store, client, "destination", nil, nil)
			runDiscovery(t, worker, 1)
			op, ok := oldOperation.Load().(string)
			require.True(ok)
			_, err = f.Store.DB().Exec(`UPDATE recording_references SET last_send_at='2000-01-01 00:00:00.000'`)
			require.NoError(err)
			claims, err := f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
			require.NoError(err)
			require.Len(claims, 1)
			lastSend := claims[0].LastSendAt
			expectedKey.Store("new-key")
			if !strings.HasSuffix(code, "unauthorized") {
				t.Setenv("MSGVAULT_REFERENCE_TEST_KEY", "new-key")
			}
			lookup := strings.HasPrefix(code, "receipt_")
			if lookup {
				recordingBody(t, f, id, "https://loom.com/share/abc?token=new")
			}
			fail.Store(strings.HasSuffix(code, "server_error"))
			missingKey.Store(code == "credential_unavailable")
			due(t, f)
			require.NoError(worker.RunBatch(t.Context()))
			state, gotCode, current := recordingState(t, f, id)
			assert.Equal("uncertain", state)
			assert.Equal(code, gotCode)
			assert.Equal(op, current)
			claims, err = f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
			require.NoError(err)
			require.Len(claims, 1)
			if lookup || code == "credential_unavailable" {
				assert.Equal(lastSend, claims[0].LastSendAt)
			}
			t.Setenv("MSGVAULT_REFERENCE_TEST_KEY", "new-key")
			fail.Store(false)
			missingKey.Store(false)
			recordingBody(t, f, id, "https://loom.com/share/abc?token=new")
			due(t, f)
			require.NoError(worker.RunBatch(t.Context()))
			wantGets := 1
			if lookup {
				wantGets++
			}
			require.Len(gets, wantGets)
			for range wantGets {
				assert.Equal("/api/v1/media/operations/"+op, <-gets)
			}
		})
	}
	for _, tc := range []struct {
		name       string
		live       bool
		status     int
		old        bool
		want, code string
	}{
		{"resend", true, 200, false, "retained", ""},
		{"recent miss", false, 404, false, "uncertain", "not_found"},
		{"settled miss", false, 404, true, "withdrawn", "receipt_not_found"},
		{"rejection then settled live miss", true, 422, false, "uncertain", "validation"},
		{"old route missing", true, 404, true, "uncertain", "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := storetest.New(t)
			id := f.CreateMessage("recording")
			recordingBody(t, f, id, "https://loom.com/share/abc?token=one")
			lose := true
			var accept atomic.Bool
			var requests []docbankmedia.ReferenceRequest
			var get string
			client := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					get = r.URL.Path
					status := tc.status
					if tc.live && status != http.StatusForbidden {
						status = http.StatusNotFound
					}
					w.WriteHeader(status)
					if tc.status == 200 {
						writeReceipt(t, w, strings.TrimPrefix(get, "/api/v1/media/operations/"))
					}
					return
				}
				var req docbankmedia.ReferenceRequest
				if !assert.NoError(json.UnmarshalRead(r.Body, &req)) {
					return
				}
				requests = append(requests, req)
				if lose {
					hijacker, ok := w.(http.Hijacker)
					if !assert.True(ok) {
						return
					}
					conn, _, err := hijacker.Hijack()
					if !assert.NoError(err) {
						return
					}
					_ = conn.Close()
					return
				}
				if tc.status != http.StatusOK && !accept.Load() {
					w.WriteHeader(tc.status)
					return
				}
				writeReceipt(t, w, req.OperationID)
			})
			w := NewWorker(f.Store, client, "destination", nil, nil)
			runDiscovery(t, w, 1)
			state, code, op := recordingState(t, f, id)
			assert.Equal("uncertain", state)
			assert.Equal("transport", code)
			if !tc.live {
				_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=? WHERE id=?`), "2000-01-01 00:00:00.000", id)
				require.NoError(err)
			}
			if tc.old {
				_, err := f.Store.DB().Exec(`UPDATE recording_references SET last_send_at='2000-01-01 00:00:00.000'`)
				require.NoError(err)
			}
			lose = false
			due(t, f)
			err := w.RunBatch(t.Context())
			require.NoError(err)
			state, code, after := recordingState(t, f, id)
			assert.Equal(tc.want, state)
			assert.Equal(tc.code, code)
			assert.Equal(op, after)
			if tc.name == "resend" {
				var outcome, coverage string
				require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT outcome,coverage_state FROM recording_references WHERE message_id=?`), id).Scan(&outcome, &coverage))
				assert.Equal("access_required", outcome)
				assert.Equal("unprocessed", coverage)
			}
			if tc.live {
				require.Len(requests, 2)
				assert.Equal(requests[0], requests[1])
				if tc.status == http.StatusUnprocessableEntity {
					claims, err := f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
					require.NoError(err)
					require.Len(claims, 1)
					sent := claims[0].LastSendAt
					due(t, f)
					err = w.RunBatch(t.Context())
					require.NoError(err)
					state, code, _ = recordingState(t, f, id)
					assert.Equal("uncertain", state)
					assert.Equal("not_found", code)
					claims, err = f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
					require.NoError(err)
					require.Len(claims, 1)
					assert.Equal(sent, claims[0].LastSendAt)
					_, err = f.Store.DB().Exec(`UPDATE recording_references SET last_send_at='2000-01-01 00:00:00.000'`)
					require.NoError(err)
					due(t, f)
					err = w.RunBatch(t.Context())
					require.NoError(err)
					state, code, _ = recordingState(t, f, id)
					assert.Equal("pending", state)
					assert.Equal("receipt_not_found", code)
					accept.Store(true)
					err = w.RunBatch(t.Context())
					require.NoError(err)
					state, code, after = recordingState(t, f, id)
					assert.Equal("retained", state)
					assert.Empty(code)
					assert.Equal(op, after)
					require.Len(requests, 3)
					assert.Equal(requests[0], requests[2])
				}
			} else {
				assert.Equal("/api/v1/media/operations/"+op, get)
			}
		})
	}
}

func TestRecordingReferenceSecrets(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("recording")
	require.NoError(f.Store.UpsertMessageBody(id, sql.NullString{String: "Watch recording", Valid: true}, sql.NullString{String: `<a href="https://cap.example.test/s/path_secret?token=query_secret#fragment_secret">Watch recording</a>`, Valid: true}))
	var logs bytes.Buffer
	client := recordingClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"query_secret","detail":"path_secret"}`))
	})
	w := NewWorker(f.Store, client, "destination", []string{"https://cap.example.test"}, slog.New(slog.NewTextHandler(&logs, nil)))
	runDiscovery(t, w, 1)
	var row string
	require.NoError(f.Store.DB().QueryRow(`SELECT route_key || ref_sha256 || operation_id || occurrence_json || state || error_code || source_id || occurrence_id || outcome || coverage_state FROM recording_references`).Scan(&row))
	for _, secret := range []string{"path_secret", "query_secret", "fragment_secret"} {
		assert.NotContains(row, secret)
		assert.NotContains(logs.String(), secret)
	}
}

func TestRecordingReferenceOccurrenceCorrection(t *testing.T) {
	for _, tc := range []struct{ field, settlement string }{
		{"sent_at", "retained"},
		{"source_message_id", "rollback"},
	} {
		t.Run(tc.field+"/"+tc.settlement, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := storetest.New(t)
			id := f.NewMessage().WithSourceMessageID("original").WithSentAt(time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)).Create(t, f.Store)
			recordingBody(t, f, id, "https://loom.com/share/abc")
			requests := make(chan docbankmedia.ReferenceRequest, 5)
			var ready atomic.Bool
			ready.Store(tc.settlement == "retained")
			client := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
				var req docbankmedia.ReferenceRequest
				if !assert.NoError(json.UnmarshalRead(r.Body, &req)) {
					return
				}
				requests <- req
				if !ready.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				writeReceipt(t, w, req.OperationID)
			})
			worker := NewWorker(f.Store, client, "destination", nil, nil)
			runDiscovery(t, worker, 1)
			require.Len(requests, 1)
			original := <-requests
			value := "corrected"
			if tc.field == "sent_at" {
				value = "2026-01-01 10:00:00.123"
			}
			_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET `+tc.field+`=? WHERE id=?`), value, id)
			require.NoError(err)
			waitForRecordingChange(t, worker, id)
			if tc.settlement == "rollback" {
				before, err := f.Store.LoadRecordingReferenceCursor(t.Context(), "destination")
				require.NoError(err)
				restore := rejectRecordingReplacement(t, f)
				t.Cleanup(restore)
				ready.Store(true)
				due(t, f)
				err = worker.RunBatch(t.Context())
				require.ErrorContains(err, "injected recording replacement failure")
				require.Len(requests, 1)
				assert.Equal(original, <-requests)
				after, err := f.Store.LoadRecordingReferenceCursor(t.Context(), "destination")
				require.NoError(err)
				assert.False(after.At.Before(before.At))
				assert.Equal(before.Policy, after.Policy)
				restore()
				due(t, f)
				require.NoError(worker.RunBatch(t.Context()))
				require.Len(requests, 1)
				assert.Equal(original, <-requests)
				return
			}
			require.Len(requests, 1)
			corrected := <-requests
			assert.Equal(original.ReferenceURL, corrected.ReferenceURL)
			assert.NotEqual(original.Occurrence.Revision, corrected.Occurrence.Revision)
			assert.Equal("2026-01-01T10:00:00.123Z", corrected.Occurrence.Message.Normalized)
			assert.Equal(3, corrected.Occurrence.Message.FractionDigits)
			require.NotNil(corrected.Occurrence.Message.OffsetSeconds)
			assert.Zero(*corrected.Occurrence.Message.OffsetSeconds)
			assert.Equal(original.Occurrence.Ref, corrected.Occurrence.Ref)
			require.NoError(worker.RunBatch(t.Context()))
			assert.Empty(requests)
		})
	}
}

func rejectRecordingReplacement(t *testing.T, f *storetest.Fixture) func() {
	t.Helper()
	create := `CREATE TRIGGER reject_recording_replacement BEFORE UPDATE OF operation_id ON recording_references WHEN NEW.operation_id <> OLD.operation_id BEGIN SELECT RAISE(ABORT, 'injected recording replacement failure'); END`
	drop := `DROP TRIGGER IF EXISTS reject_recording_replacement`
	if f.Store.IsPostgreSQL() {
		create = `CREATE FUNCTION reject_recording_replacement() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected recording replacement failure'; END; $$ LANGUAGE plpgsql;
CREATE TRIGGER reject_recording_replacement BEFORE UPDATE OF operation_id ON recording_references FOR EACH ROW WHEN (NEW.operation_id <> OLD.operation_id) EXECUTE FUNCTION reject_recording_replacement()`
		drop = `DROP TRIGGER IF EXISTS reject_recording_replacement ON recording_references; DROP FUNCTION IF EXISTS reject_recording_replacement()`
	}
	_, err := f.Store.DB().Exec(create)
	require.NoError(t, err)
	return func() {
		_, err := f.Store.DB().Exec(drop)
		require.NoError(t, err)
	}
}

func TestRecordingReferenceCorrectionDuringHTTP(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnprocessableEntity} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := storetest.New(t)
			id := f.CreateMessage("original")
			recordingBody(t, f, id, "https://loom.com/share/abc")
			_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET content_changed_at='2000-01-01 00:00:00.000' WHERE id=?`), id)
			require.NoError(err)
			requests := make(chan docbankmedia.ReferenceRequest, 5)
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			var posts atomic.Int32
			client := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
				var req docbankmedia.ReferenceRequest
				if !assert.NoError(json.UnmarshalRead(r.Body, &req)) {
					return
				}
				requests <- req
				if posts.Add(1) == 1 {
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					if status != http.StatusOK {
						w.WriteHeader(status)
						return
					}
				}
				writeReceipt(t, w, req.OperationID)
			})
			var mu sync.Mutex
			gate := func(context.Context) (func(), bool) { mu.Lock(); return mu.Unlock, true }
			worker := NewWorker(f.Store, client, "destination", nil, nil).WithOperationGate(gate)
			done := make(chan error, 1)
			go func() {
				done <- worker.RunBatch(t.Context())
			}()
			var original docbankmedia.ReferenceRequest
			select {
			case original = <-requests:
			case <-time.After(5 * time.Second):
				require.FailNow("request did not reach HTTP")
			}
			mu.Lock()
			_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET source_message_id=? WHERE id=?`), "corrected", id)
			mu.Unlock()
			require.NoError(err)
			discovery := NewWorker(f.Store, client, "destination", nil, nil).WithOperationGate(gate)
			waitForRecordingChange(t, discovery, id)
			unblock()
			select {
			case err := <-done:
				require.NoError(err)
			case <-time.After(5 * time.Second):
				require.FailNow("request completion did not settle")
			}
			state, _, op := recordingState(t, f, id)
			assert.Equal("pending", state)
			assert.NotEqual(original.OperationID, op)
			cursor, err := f.Store.LoadRecordingReferenceCursor(t.Context(), "destination")
			require.NoError(err)
			remaining, err := f.Store.ListChangedMessages(t.Context(), cursor.FeedCursor(), 200)
			require.NoError(err)
			assert.Empty(remaining.Messages)
			require.NoError(discovery.RunBatch(t.Context()))
			require.Len(requests, 1)
			corrected := <-requests
			assert.Equal(op, corrected.OperationID)
			assert.NotEqual(original.Occurrence.Ref, corrected.Occurrence.Ref)
			err = discovery.RunBatch(t.Context())
			require.NoError(err)
			assert.Empty(requests)
		})
	}
}
