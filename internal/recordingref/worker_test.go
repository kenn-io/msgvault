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
	assert.Eventually(t, func() bool {
		err := w.RunBatch(t.Context())
		require.NoError(t, err)
		var got int
		require.NoError(t, w.st.DB().QueryRow(`SELECT COUNT(*) FROM recording_references`).Scan(&got))
		return got == count
	}, 5*time.Second, 10*time.Millisecond)
}

func waitForRecordingChange(t *testing.T, w *Worker, id int64) {
	t.Helper()
	assert, require := assert.New(t), require.New(t)
	before, err := w.st.LoadRecordingReferenceCursor(t.Context(), w.destination)
	require.NoError(err)
	assert.Eventually(func() bool {
		changed, err := w.st.ListChangedMessages(t.Context(), before.FeedCursor(), 200)
		require.NoError(err)
		require.NoError(w.RunBatch(t.Context()))
		after, err := w.st.LoadRecordingReferenceCursor(t.Context(), w.destination)
		require.NoError(err)
		for _, message := range changed.Messages {
			if message.ID == id {
				return after.At.After(message.ContentChangedAt) || after.At.Equal(message.ContentChangedAt) && (!after.AfterRow || after.AfterID >= id)
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)
}

func recordingClient(t *testing.T, handler http.HandlerFunc) *docbankmedia.Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	c, err := docbankmedia.NewClient(s.URL, nil)
	require.NoError(t, err)
	return c
}

func writeReceipt(w http.ResponseWriter, op string) {
	data, _ := json.Marshal(docbankmedia.Receipt{VaultUID: "vault", SourceID: "source", OperationID: op, OccurrenceID: "occurrence", OperationState: "succeeded", CoverageState: "unprocessed", Outcome: "access_required"})
	_, _ = w.Write(data)
}

func TestRecordingReferenceFeed(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	client := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req docbankmedia.ReferenceRequest
		if !assert.NoError(json.UnmarshalRead(r.Body, &req)) {
			return
		}
		writeReceipt(w, req.OperationID)
	})
	one := f.CreateMessage("one")
	recordingBody(t, f, one, "https://loom.com/share/one")
	w := NewWorker(f.Store, client, "destination", nil, nil)
	runDiscovery(t, w, 1)
	source, err := f.Store.GetOrCreateSource("slack", "alice")
	require.NoError(err)
	conv, err := f.Store.EnsureConversation(source.ID, "thread", "Thread")
	require.NoError(err)
	two, err := f.Store.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "two", MessageType: "slack"})
	require.NoError(err)
	recordingBody(t, f, two, "https://cap.example.test/s/two")
	err = w.RunBatch(t.Context())
	require.NoError(err)
	w = NewWorker(f.Store, client, "destination", []string{"https://cap.example.test"}, nil)
	runDiscovery(t, w, 2)
	recordingBody(t, f, one, "recording removed")
	assert.Eventually(func() bool {
		err := w.RunBatch(t.Context())
		require.NoError(err)
		state, _, _ := recordingState(t, f, one)
		return state == "withdrawn"
	}, 5*time.Second, 10*time.Millisecond)
}

func TestRecordingReferenceFeedHTML(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("html-recording")
	text := "Watch recording"
	body := `<p><a href="https://loom.com/share/abc?token=one&amp;key=two!">Watch recording</a></p><a href="https://loom.com/share/abc?token=one&amp;key=two!">Again</a>`
	require.NoError(f.Store.UpsertMessageBody(id, sql.NullString{String: text, Valid: true}, sql.NullString{String: body, Valid: true}))
	requests := make(chan docbankmedia.ReferenceRequest, 5)
	client := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req docbankmedia.ReferenceRequest
		if !assert.NoError(json.UnmarshalRead(r.Body, &req)) {
			return
		}
		requests <- req
		writeReceipt(w, req.OperationID)
	})
	w := NewWorker(f.Store, client, "destination", nil, nil)
	runDiscovery(t, w, 1)
	require.Len(requests, 1)
	assert.Equal("https://loom.com/share/abc?token=one&key=two!", (<-requests).ReferenceURL)
	savedText, savedHTML := f.GetMessageBody(id)
	assert.Equal(text, savedText.String)
	assert.Equal(body, savedHTML.String)
	recordingText := "Watch recording https://loom.com/share/abc?token=one&key=two!"
	require.NoError(f.Store.UpsertMessageBody(id, sql.NullString{String: recordingText, Valid: true}, sql.NullString{String: body, Valid: true}))
	m, exists, err := f.Store.ReadRecordingMessage(t.Context(), id)
	require.NoError(err)
	require.True(exists)
	require.Len(messageRefs(m, nil), 1)
	waitForRecordingChange(t, w, id)
	assert.Empty(requests)
}

func TestRecordingReferenceFeedHTMLVisibleURL(t *testing.T) {
	for _, tc := range []struct {
		name, href, before, after string
		extraAnchors              int
	}{
		{"exact", "https://cap.so/s/abc!", "", "", 0},
		{"period", "https://cap.so/s/abc!", "", ".", 0},
		{"parenthesized", "https://cap.so/s/abc!", "(", ")", 0},
		{"parenthesis ID", "https://cap.so/s/abc)", "", ".", 0},
		{"parenthesized parenthesis ID", "https://cap.so/s/abc)", "(", ")", 0},
		{"long suffix", "https://cap.so/s/abc!", "", strings.Repeat(".", 1<<20), 8},
	} {
		for _, htmlOnly := range []bool{false, true} {
			t.Run(tc.name+"/htmlOnly="+strconv.FormatBool(htmlOnly), func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				f := storetest.New(t)
				id := f.CreateMessage("html-visible-url")
				visible := tc.before + tc.href + tc.after
				text := sql.NullString{String: visible + " https://cap.so/s/other", Valid: true}
				body := tc.before + `<a href="` + tc.href + `">` + tc.href + `</a>` + tc.after + " https://cap.so/s/other"
				want := []string{tc.href, "https://cap.so/s/other"}
				var anchors strings.Builder
				for i := range tc.extraAnchors {
					href := "https://cap.so/s/extra" + strconv.Itoa(i)
					anchors.WriteString(` <a href="` + href + `">recording</a>`)
					want = append(want, href)
				}
				body += anchors.String()
				text.String += strings.Repeat(" recording", tc.extraAnchors)
				if htmlOnly {
					text = sql.NullString{}
				}
				require.NoError(f.Store.UpsertMessageBody(id, text, sql.NullString{String: body, Valid: true}))
				m, found, err := f.Store.ReadRecordingMessage(t.Context(), id)
				require.NoError(err)
				require.True(found)
				refs := messageRefs(m, nil)
				var got []string
				for _, ref := range refs {
					got = append(got, ref.Reference)
				}
				assert.ElementsMatch(want, got)
				requests := make(chan string, len(want)+1)
				client := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
					var req docbankmedia.ReferenceRequest
					if !assert.NoError(json.UnmarshalRead(r.Body, &req)) {
						return
					}
					requests <- req.ReferenceURL
					writeReceipt(w, req.OperationID)
				})
				w := NewWorker(f.Store, client, "destination", nil, nil)
				runDiscovery(t, w, len(want))
				require.Len(requests, len(want))
				got = nil
				for range want {
					got = append(got, <-requests)
				}
				assert.ElementsMatch(want, got)
				savedText, savedHTML := f.GetMessageBody(id)
				assert.Equal(text, savedText)
				assert.Equal(body, savedHTML.String)
				for i, independent := range []string{"https://cap.so/s/abc", "https://cap.so/s/abc!extra"} {
					if htmlOnly {
						body += " " + independent
					} else {
						text.String += " " + independent
					}
					require.NoError(f.Store.UpsertMessageBody(id, text, sql.NullString{String: body, Valid: true}))
					runDiscovery(t, w, len(want)+1+i)
					require.Len(requests, 1)
					assert.Equal(independent, <-requests)
				}
			})
		}
	}
}

func TestRecordingReferenceBackfill(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	for i := range 405 {
		id := f.CreateMessage(strconv.Itoa(i))
		recordingBody(t, f, id, "https://loom.com/share/"+strconv.Itoa(i))
	}
	_, err := f.Store.DB().Exec(`UPDATE messages SET content_changed_at='2000-01-01 00:00:00.000'`)
	require.NoError(err)
	client := recordingClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	err = NewWorker(f.Store, client, "destination", nil, nil).RunBatch(t.Context())
	require.NoError(err)
	cursor, err := f.Store.LoadRecordingReferenceCursor(t.Context(), "destination")
	require.NoError(err)
	assert.True(cursor.At.After(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)))
	assert.False(cursor.AfterRow)
	var count int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM recording_references`).Scan(&count))
	assert.Equal(405, count)
}

func TestRecordingReferenceTeamsPointer(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("teams")
	require.NoError(t, f.Store.ReplaceMessageLinkAttachments(id, []store.AttachmentRef{{SourceAttachmentID: "teams:recording:abc", StoragePath: "https://teams.example.test/play?token=secret"}}))
	client := recordingClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	runDiscovery(t, NewWorker(f.Store, client, "destination", nil, nil), 1)
	state, _, _ := recordingState(t, f, id)
	assert.Equal(t, "uncertain", state)
}

func TestRecordingReferenceDelivery(t *testing.T) {
	t.Run("hidden until restored", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		f := storetest.New(t)
		id := f.CreateMessage("recording")
		recordingBody(t, f, id, "https://loom.com/share/abc")
		requests := 0
		client := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
			var req docbankmedia.ReferenceRequest
			if !assert.NoError(json.UnmarshalRead(r.Body, &req)) {
				return
			}
			requests++
			writeReceipt(w, req.OperationID)
		})
		m, exists, err := f.Store.ReadRecordingMessage(t.Context(), id)
		require.NoError(err)
		require.True(exists)
		inputs, err := referenceInputs(m, messageRefs(m, nil))
		require.NoError(err)
		require.NoError(f.Store.ReconcileRecordingReferences(t.Context(), "destination", id, true, inputs))
		_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_at=? WHERE id=?`), "2000-01-01 00:00:00.000", id)
		require.NoError(err)
		w := NewWorker(f.Store, client, "destination", nil, nil)
		err = w.RunBatch(t.Context())
		require.NoError(err)
		assert.Zero(requests)
		state, _, _ := recordingState(t, f, id)
		assert.Equal("withdrawn", state)
		_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_at=NULL WHERE id=?`), id)
		require.NoError(err)
		assert.Eventually(func() bool { err := w.RunBatch(t.Context()); require.NoError(err); return requests == 1 }, 5*time.Second, 10*time.Millisecond)
	})
	for _, tc := range []struct {
		name        string
		status      int
		code, state string
	}{
		{"retry", 503, "server_error", "uncertain"},
		{"blocked", 422, "validation", "blocked"},
		{"capability", 503, "capability_unavailable", "uncertain"},
		{"credential", 0, "credential_unavailable", "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := storetest.New(t)
			id := f.CreateMessage("recording")
			recordingBody(t, f, id, "https://loom.com/share/abc?token=one")
			var requests []docbankmedia.ReferenceRequest
			status := tc.status
			client := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
				var req docbankmedia.ReferenceRequest
				if !assert.NoError(json.UnmarshalRead(r.Body, &req)) {
					return
				}
				requests = append(requests, req)
				if status != 200 {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"code":"` + tc.code + `"}`))
					return
				}
				writeReceipt(w, req.OperationID)
			})
			if tc.name == "credential" {
				client, err := docbankmedia.NewClient("http://127.0.0.1:1", func() (string, error) { return "", docbankmedia.ErrCredentialUnavailable })
				require.NoError(err)
				w := NewWorker(f.Store, client, "destination", nil, nil)
				runDiscovery(t, w, 1)
				state, code, _ := recordingState(t, f, id)
				assert.Equal(tc.state, state)
				assert.Equal(tc.code, code)
				assert.Empty(requests)
				return
			}
			w := NewWorker(f.Store, client, "destination", nil, nil)
			runDiscovery(t, w, 1)
			state, code, operation := recordingState(t, f, id)
			assert.Equal(tc.state, state)
			assert.Equal(tc.code, code)
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
			status = 200
			require.NoError(f.Store.ReconsiderBlockedRecordingReferences(t.Context(), "destination"))
			due(t, f)
			err := w.RunBatch(t.Context())
			require.NoError(err)
			state, code, after := recordingState(t, f, id)
			assert.Equal("retained", state)
			var outcome, coverage string
			require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT outcome,coverage_state FROM recording_references WHERE message_id=?`), id).Scan(&outcome, &coverage))
			assert.Equal("access_required", outcome)
			assert.Equal("unprocessed", coverage)
			assert.Empty(code)
			assert.Equal(operation, after)
			assert.Equal(requests[0], requests[len(requests)-1])
			recordingBody(t, f, id, "https://loom.com/share/abc?token=two")
			assert.Eventually(func() bool {
				err := w.RunBatch(t.Context())
				require.NoError(err)
				_, _, after := recordingState(t, f, id)
				return after != operation
			}, 5*time.Second, 10*time.Millisecond)
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
					writeReceipt(w, oldOperation)
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
				writeReceipt(w, req.OperationID)
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
	t.Run("key rotation and replacement recovers committed receipt", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		f := storetest.New(t)
		id := f.CreateMessage("recording")
		recordingBody(t, f, id, "https://loom.com/share/abc?token=old")
		t.Setenv("MSGVAULT_REFERENCE_TEST_KEY", "old-key")
		var expectedKey, oldOperation atomic.Value
		expectedKey.Store("old-key")
		var lost, failReceipt atomic.Bool
		gets := make(chan string, 10)
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
			if r.Method == http.MethodGet {
				if failReceipt.CompareAndSwap(true, false) {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				writeReceipt(w, strings.TrimPrefix(r.URL.Path, "/api/v1/media/operations/"))
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
			writeReceipt(w, req.OperationID)
		}))
		defer server.Close()
		client, err := docbankmedia.NewClient(server.URL, func() (string, error) { return os.Getenv("MSGVAULT_REFERENCE_TEST_KEY"), nil })
		require.NoError(err)
		worker := NewWorker(f.Store, client, "destination", nil, nil)
		runDiscovery(t, worker, 1)
		op, ok := oldOperation.Load().(string)
		require.True(ok)
		expectedKey.Store("new-key")
		_, err = f.Store.DB().Exec(`UPDATE recording_references SET last_send_at='2000-01-01 00:00:00.000'`)
		require.NoError(err)
		due(t, f)
		err = worker.RunBatch(t.Context())
		require.NoError(err)
		state, code, current := recordingState(t, f, id)
		assert.Equal("uncertain", state)
		assert.Equal("unauthorized", code)
		assert.Equal(op, current)
		recordingBody(t, f, id, "https://loom.com/share/abc?token=new")
		due(t, f)
		err = worker.RunBatch(t.Context())
		require.NoError(err)
		state, code, current = recordingState(t, f, id)
		assert.Equal("uncertain", state)
		assert.Equal("receipt_unauthorized", code)
		assert.Equal(op, current)
		claims, err := f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
		require.NoError(err)
		require.Len(claims, 1)
		lastSend := claims[0].LastSendAt
		t.Setenv("MSGVAULT_REFERENCE_TEST_KEY", "new-key")
		failReceipt.Store(true)
		due(t, f)
		err = worker.RunBatch(t.Context())
		require.NoError(err)
		state, code, current = recordingState(t, f, id)
		assert.Equal("uncertain", state)
		assert.Equal("receipt_server_error", code)
		assert.Equal(op, current)
		claims, err = f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
		require.NoError(err)
		require.Len(claims, 1)
		assert.Equal(lastSend, claims[0].LastSendAt)
		due(t, f)
		err = worker.RunBatch(t.Context())
		require.NoError(err)
		require.Len(gets, 3)
		assert.Equal("/api/v1/media/operations/"+op, <-gets)
		assert.Equal("/api/v1/media/operations/"+op, <-gets)
		assert.Equal("/api/v1/media/operations/"+op, <-gets)
		state, _, current = recordingState(t, f, id)
		assert.Equal("pending", state)
		assert.NotEqual(op, current)
		due(t, f)
		err = worker.RunBatch(t.Context())
		require.NoError(err)
		state, _, current = recordingState(t, f, id)
		assert.Equal("retained", state)
		assert.NotEqual(op, current)
	})
	t.Run("unsuccessful resend preserves receipt recovery", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		f := storetest.New(t)
		id := f.CreateMessage("recording")
		recordingBody(t, f, id, "https://loom.com/share/abc")
		m, exists, err := f.Store.ReadRecordingMessage(t.Context(), id)
		require.NoError(err)
		require.True(exists)
		inputs, err := referenceInputs(m, messageRefs(m, nil))
		require.NoError(err)
		require.NoError(f.Store.ReconcileRecordingReferences(t.Context(), "destination", id, true, inputs))
		claims, err := f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
		require.NoError(err)
		require.Len(claims, 1)
		now := time.Now().UTC()
		applied, err := f.Store.FinishRecordingReference(t.Context(), claims[0], store.RecordingReferenceResult{State: "uncertain", LastSendAt: &now, NextActionAt: now}, true, inputs)
		require.NoError(err)
		require.True(applied)
		client := recordingClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
		w := NewWorker(f.Store, client, "destination", nil, nil)
		err = w.RunBatch(t.Context())
		require.NoError(err)
		state, code, op := recordingState(t, f, id)
		assert.Equal("uncertain", state)
		assert.Equal("server_error", code)
		assert.Equal(claims[0].OperationID, op)
		claims, err = f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
		require.NoError(err)
		require.Len(claims, 1)
		lastSend := claims[0].LastSendAt
		missingKey, err := docbankmedia.NewClient("http://127.0.0.1:1", func() (string, error) { return "", docbankmedia.ErrCredentialUnavailable })
		require.NoError(err)
		w.client = missingKey
		due(t, f)
		err = w.RunBatch(t.Context())
		require.NoError(err)
		state, code, _ = recordingState(t, f, id)
		assert.Equal("uncertain", state)
		assert.Equal("credential_unavailable", code)
		claims, err = f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
		require.NoError(err)
		require.Len(claims, 1)
		assert.Equal(lastSend, claims[0].LastSendAt)
		recordingBody(t, f, id, "link removed")
		due(t, f)
		w.client = recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(http.MethodGet, r.Method)
			writeReceipt(w, op)
		})
		err = w.RunBatch(t.Context())
		require.NoError(err)
		state, code, _ = recordingState(t, f, id)
		assert.Equal("withdrawn", state)
		assert.Empty(code)
	})
	for _, tc := range []struct {
		name       string
		live       bool
		status     int
		old        bool
		want, code string
	}{
		{"resend", true, 200, false, "retained", ""},
		{"receipt", false, 200, false, "withdrawn", ""},
		{"recent miss", false, 404, false, "uncertain", "not_found"},
		{"settled miss", false, 404, true, "withdrawn", "receipt_not_found"},
		{"recent rejection", true, 422, false, "uncertain", "validation"},
		{"old rejection", true, 422, true, "uncertain", "validation"},
		{"old route missing", true, 404, true, "uncertain", "not_found"},
		{"old method rejection", true, 405, true, "uncertain", "http_error"},
		{"old forbidden", true, 403, true, "uncertain", "forbidden"},
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
						writeReceipt(w, strings.TrimPrefix(get, "/api/v1/media/operations/"))
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
				writeReceipt(w, req.OperationID)
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
			if tc.live {
				require.Len(requests, 2)
				assert.Equal(requests[0], requests[1])
				if tc.status == http.StatusUnprocessableEntity || tc.status == http.StatusNotFound || tc.status == http.StatusMethodNotAllowed {
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
	_, err := client.SubmitReference(t.Context(), docbankmedia.ReferenceRequest{OperationID: "op", ReferenceURL: "https://cap.example.test/s/path_secret"})
	require.Error(err)
	assert.NotContains(err.Error(), "secret")
}

func TestRecordingReferenceOccurrenceCorrection(t *testing.T) {
	for _, field := range []string{"sent_at", "source_message_id"} {
		for _, settlement := range []string{"retained", "replay", "receipt"} {
			t.Run(field+"/"+settlement, func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				f := storetest.New(t)
				id := f.NewMessage().WithSourceMessageID("original").WithSentAt(time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)).Create(t, f.Store)
				recordingBody(t, f, id, "https://loom.com/share/abc")
				requests := make(chan docbankmedia.ReferenceRequest, 5)
				gets := make(chan string, 2)
				var posts atomic.Int32
				var ready atomic.Bool
				ready.Store(settlement == "retained")
				client := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						gets <- r.URL.Path
						writeReceipt(w, strings.TrimPrefix(r.URL.Path, "/api/v1/media/operations/"))
						ready.Store(true)
						return
					}
					var req docbankmedia.ReferenceRequest
					if !assert.NoError(json.UnmarshalRead(r.Body, &req)) {
						return
					}
					requests <- req
					post := posts.Add(1)
					if !ready.Load() {
						if settlement == "receipt" && post > 1 {
							w.WriteHeader(http.StatusUnauthorized)
						} else {
							w.WriteHeader(http.StatusServiceUnavailable)
						}
						return
					}
					writeReceipt(w, req.OperationID)
				})
				worker := NewWorker(f.Store, client, "destination", nil, nil)
				runDiscovery(t, worker, 1)
				require.Len(requests, 1)
				original := <-requests
				value := "corrected"
				if field == "sent_at" {
					value = "2026-01-01 10:00:00.000"
				}
				_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET `+field+`=? WHERE id=?`), value, id)
				require.NoError(err)
				waitForRecordingChange(t, worker, id)
				if settlement != "retained" {
					state, _, op := recordingState(t, f, id)
					assert.Equal("uncertain", state)
					assert.Equal(original.OperationID, op)
					assert.Empty(requests)
					if settlement == "replay" {
						ready.Store(true)
					}
					due(t, f)
					err = worker.RunBatch(t.Context())
					require.NoError(err)
					if settlement == "receipt" {
						require.Len(requests, 1)
						assert.Equal(original, <-requests)
						due(t, f)
						err = worker.RunBatch(t.Context())
						require.NoError(err)
						require.Len(gets, 1)
						assert.Equal("/api/v1/media/operations/"+original.OperationID, <-gets)
					} else {
						require.Len(requests, 1)
						assert.Equal(original, <-requests)
					}
					state, _, op = recordingState(t, f, id)
					assert.Equal("pending", state)
					assert.NotEqual(original.OperationID, op)
					err = worker.RunBatch(t.Context())
					require.NoError(err)
				}
				require.Len(requests, 1)
				corrected := <-requests
				assert.NotEqual(original.OperationID, corrected.OperationID)
				assert.Equal(original.ReferenceURL, corrected.ReferenceURL)
				if field == "sent_at" {
					assert.NotEqual(original.Occurrence.Revision, corrected.Occurrence.Revision)
					assert.Equal("2026-01-01T10:00:00Z", corrected.Occurrence.Message.Normalized)
					assert.Equal(original.Occurrence.Ref, corrected.Occurrence.Ref)
				} else {
					assert.NotEqual(original.Occurrence.Ref, corrected.Occurrence.Ref)
					assert.Equal(original.Occurrence.Revision, corrected.Occurrence.Revision)
				}
				err = worker.RunBatch(t.Context())
				require.NoError(err)
				assert.Empty(requests)
				assert.Empty(gets)
			})
		}
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

func TestRecordingReferenceSettlementRollback(t *testing.T) {
	for _, field := range []string{"sent_at", "source_message_id"} {
		t.Run(field, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := storetest.New(t)
			id := f.NewMessage().WithSourceMessageID("original").WithSentAt(time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)).Create(t, f.Store)
			recordingBody(t, f, id, "https://loom.com/share/abc")
			requests := make(chan docbankmedia.ReferenceRequest, 5)
			var ready atomic.Bool
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
				writeReceipt(w, req.OperationID)
			})
			worker := NewWorker(f.Store, client, "destination", nil, nil)
			runDiscovery(t, worker, 1)
			require.Len(requests, 1)
			original := <-requests
			value := "corrected"
			if field == "sent_at" {
				value = "2026-01-01 10:00:00.000"
			}
			_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET `+field+`=? WHERE id=?`), value, id)
			require.NoError(err)
			waitForRecordingChange(t, worker, id)
			before, err := f.Store.LoadRecordingReferenceCursor(t.Context(), "destination")
			require.NoError(err)
			claims, err := f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
			require.NoError(err)
			require.Len(claims, 1)
			frozen := claims[0]
			restore := rejectRecordingReplacement(t, f)
			t.Cleanup(restore)
			ready.Store(true)
			due(t, f)
			err = worker.RunBatch(t.Context())
			require.ErrorContains(err, "injected recording replacement failure")
			require.Len(requests, 1)
			assert.Equal(original, <-requests)
			claims, err = f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
			require.NoError(err)
			require.Len(claims, 1)
			assert.Equal("uncertain", claims[0].State)
			assert.Equal(frozen.OperationID, claims[0].OperationID)
			assert.Equal(frozen.OccurrenceJSON, claims[0].OccurrenceJSON)
			assert.Equal(frozen.ErrorCode, claims[0].ErrorCode)
			assert.Equal(frozen.RetryCount, claims[0].RetryCount)
			var receipt string
			require.NoError(f.Store.DB().QueryRow(`SELECT occurrence_id FROM recording_references`).Scan(&receipt))
			assert.Empty(receipt)
			restore()
			worker = NewWorker(f.Store, client, "destination", nil, nil)
			due(t, f)
			err = worker.RunBatch(t.Context())
			require.NoError(err)
			remaining, err := f.Store.ListChangedMessages(t.Context(), before.FeedCursor(), 200)
			require.NoError(err)
			assert.Empty(remaining.Messages)
			require.Len(requests, 1)
			assert.Equal(original, <-requests)
			state, _, op := recordingState(t, f, id)
			assert.Equal("pending", state)
			assert.NotEqual(original.OperationID, op)
			err = worker.RunBatch(t.Context())
			require.NoError(err)
			require.Len(requests, 1)
			corrected := <-requests
			assert.Equal(op, corrected.OperationID)
			assert.NotEqual(original.Occurrence, corrected.Occurrence)
			after, err := f.Store.LoadRecordingReferenceCursor(t.Context(), "destination")
			require.NoError(err)
			assert.False(after.At.Before(before.At))
			assert.Equal(before.Policy, after.Policy)
			err = worker.RunBatch(t.Context())
			require.NoError(err)
			assert.Empty(requests)
		})
	}
}

func TestRecordingReferenceCorrectionDuringHTTP(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnprocessableEntity} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := storetest.New(t)
			id := f.CreateMessage("original")
			recordingBody(t, f, id, "https://loom.com/share/abc")
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
				writeReceipt(w, req.OperationID)
			})
			var mu sync.Mutex
			gate := func(context.Context) (func(), bool) { mu.Lock(); return mu.Unlock, true }
			worker := NewWorker(f.Store, client, "destination", nil, nil).WithOperationGate(gate)
			done := make(chan error, 1)
			go func() {
				for {
					err := worker.RunBatch(t.Context())
					if err != nil || posts.Load() > 0 {
						done <- err
						return
					}
					select {
					case <-time.After(10 * time.Millisecond):
					case <-t.Context().Done():
						done <- t.Context().Err()
						return
					}
				}
			}()
			var original docbankmedia.ReferenceRequest
			select {
			case original = <-requests:
			case <-time.After(5 * time.Second):
				require.FailNow("request did not reach HTTP")
			}
			mu.Lock()
			_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET source_message_id=? WHERE id=?`), "corrected", id)
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
