package beeper

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"github.com/stretchr/testify/require"
)

func TestBeeperArchiveOneShot(t *testing.T) {
	for _, status := range []int{204, 307, 308, 403, 429, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			assert := Assert.New(t)
			require := require.New(t)
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts++
				assert.Equal(http.MethodPost, r.Method)
				assert.Equal("/v1/chats/room%2Fchild/archive", r.URL.EscapedPath())
				assert.Equal("Bearer test-token", r.Header.Get("Authorization"))
				body, err := io.ReadAll(r.Body)
				assert.NoError(err)
				assert.JSONEq(`{"archived":true}`, string(body))
				if status == 307 || status == 308 {
					w.Header().Set("Location", "/replayed")
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			err := NewClient(server.URL, testToken, 1000).ArchiveChat(t.Context(), "room/child", true)
			if status == 204 {
				require.NoError(err)
			} else {
				require.Error(err)
				var writeErr *ArchiveWriteError
				require.ErrorAs(err, &writeErr)
				assert.Equal(status == 403 || status == 429, writeErr.Rejected())
			}
			assert.Equal(1, posts, "redirect or transient failure must never replay archive")
		})
	}
}

func TestBeeperLabelsReadOnly(t *testing.T) {
	for _, wire := range []string{`[{"id":"native-1","name":"Native reference"}]`, "unsupported", "null", "[]"} {
		t.Run(wire, func(t *testing.T) {
			assert := Assert.New(t)
			require := require.New(t)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				assert.Equal(http.MethodGet, r.Method)
				assert.Equal("/v1/labels", r.URL.Path)
				if wire == "unsupported" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, err := io.WriteString(w, wire)
				assert.NoError(err)
			}))
			defer server.Close()
			labels, err := NewClient(server.URL, testToken, 1000).ListLabels(t.Context())
			assert.Equal(1, requests)
			switch wire {
			case "unsupported":
				require.ErrorIs(err, ErrNotFound)
				assert.Nil(labels)
			case "null":
				require.Error(err, "a null catalog does not observe an empty native label list")
			case "[]":
				require.NoError(err)
				assert.NotNil(labels)
				assert.Empty(labels)
			default:
				require.NoError(err)
				assert.Equal([]Label{{ID: "native-1", Name: "Native reference"}}, labels)
			}
		})
	}
}

func TestBeeperContextRecentAndDelta(t *testing.T) {
	assert := Assert.New(t)
	require := require.New(t)
	f := newFakeBeeper(t)
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ch := &fakeChat{ID: "!context:example.test", AccountID: "signal", Type: "single", LastActivity: base.Add(time.Hour)}
	for i := range 65 {
		ch.Msgs = append(ch.Msgs, fakeMsg{ID: strconv.Itoa(i), SortKey: i, Timestamp: base.Add(time.Duration(i) * time.Second), Text: fmt.Sprintf("message %d", i), SenderID: "@synthetic:example.test"})
	}
	f.addChat(ch)
	server := f.server()
	defer server.Close()
	client := NewClient(server.URL, testToken, 1000)
	bounds := DefaultContextBounds()
	snap, err := client.CaptureContext(t.Context(), "signal", ch.ID, nil, bounds)
	require.NoError(err)
	require.True(snap.Complete, snap.Reason)
	assert.True(snap.OlderHistoryOmitted)
	require.Len(snap.Messages, 50)
	assert.Equal("64", snap.Head.MessageID)
	assert.NotEmpty(snap.Stamp)
	again, err := client.CaptureContext(t.Context(), "signal", ch.ID, nil, bounds)
	require.NoError(err)
	assert.Equal(snap.Stamp, again.Stamp)
	empty, err := client.CaptureContext(t.Context(), "signal", ch.ID, &snap.Head, bounds)
	require.NoError(err)
	require.True(empty.Complete, empty.Reason)
	assert.False(empty.Changed)
	f.appendMsg(ch.ID, fakeMsg{ID: "new", SortKey: 65, Timestamp: base.Add(2 * time.Hour), Text: "New overnight question", SenderID: "@synthetic:example.test"})
	delta, err := client.CaptureContext(t.Context(), "signal", ch.ID, &snap.Head, bounds)
	require.NoError(err)
	require.True(delta.Complete, delta.Reason)
	assert.True(delta.Changed)
	assert.Contains(fmt.Sprint(delta.Messages), "New overnight question")
}

func TestBeeperContextHoldsOverflowAndForeignIdentity(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, foreign := range []bool{false, true} {
		t.Run(strconv.FormatBool(foreign), func(t *testing.T) {
			assert := Assert.New(t)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if !strings.Contains(r.URL.Path, "/messages") {
					_, writeErr := fmt.Fprintf(w, `{"id":"room","accountID":"signal","lastActivity":%q}`, now.Format(time.RFC3339Nano))
					assert.NoError(writeErr)
					return
				}
				if foreign && r.URL.Query().Get("cursor") != "" {
					_, writeErr := io.WriteString(w, `{"items":[],"newestCursor":"","oldestCursor":""}`)
					assert.NoError(writeErr)
					return
				}
				account := "signal"
				if foreign {
					account = "another"
				}
				_, writeErr := fmt.Fprintf(w, `{"items":[{"id":"1","chatID":"room","accountID":%q,"text":"hello","timestamp":%q}],"newestCursor":"same","oldestCursor":"same","hasMore":false}`, account, now.Format(time.RFC3339Nano))
				assert.NoError(writeErr)
			}))
			defer server.Close()
			bounds := DefaultContextBounds()
			bounds.MaxRequests = 5
			got, err := NewClient(server.URL, testToken, 1000).CaptureContext(t.Context(), "signal", "room", nil, bounds)
			require.NoError(t, err)
			assert.False(got.Complete)
			assert.Equal("context_identity_or_limit", got.Reason)
			assert.LessOrEqual(requests, 5)
		})
	}
}

func TestBeeperContextBoundsProviderResponses(t *testing.T) {
	t.Run("unsupported default transport", func(t *testing.T) {
		assert := Assert.New(t)
		require := require.New(t)
		previous := http.DefaultTransport
		t.Cleanup(func() { http.DefaultTransport = previous })
		requests := 0
		http.DefaultTransport = handlerTransport(func(http.ResponseWriter, *http.Request) { requests++ })
		packet, err := NewClient("http://synthetic.invalid", testToken, 1000).CaptureContext(t.Context(), "signal", "room", nil, DefaultContextBounds())
		require.EqualError(err, "unsupported default context transport")
		assert.False(packet.Complete)
		assert.Empty(packet.Messages)
		assert.Zero(requests, "unsupported defaults must fail before provider access")
	})
	assert := Assert.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		// No Content-Length: exercise the limited body reader, not only its header check.
		flusher, ok := w.(http.Flusher)
		if !assert.True(ok, "provider response must support streaming") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		flusher.Flush()
		_, _ = fmt.Fprintf(w, `{"id":"room","accountID":"signal","title":%q,"lastActivity":"2026-11-01T00:00:00Z"}`, strings.Repeat("x", 8<<10))
	}))
	defer server.Close()
	bounds := DefaultContextBounds()
	bounds.MaxBytes = 1024
	packet, err := NewClient(server.URL, testToken, 1000).CaptureContext(t.Context(), "signal", "room", nil, bounds)
	require.NoError(t, err)
	assert.False(packet.Complete)
	assert.Equal(1, requests)
	assert.Equal("provider_unavailable", packet.Reason, "oversized transport bodies must be refused before decoding")
	assert.Empty(packet.Messages)
	t.Run("emitted envelope boundary", func(t *testing.T) {
		assert := Assert.New(t)
		require := require.New(t)
		f := newFakeBeeper(t)
		base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		chat := &fakeChat{ID: "!bounded:example.test", AccountID: "signal", Type: "single", Title: "Bounded context", LastActivity: base,
			Msgs: []fakeMsg{{ID: "event", SortKey: 1, Timestamp: base, Text: "Review this question", SenderID: "@synthetic:example.test"}}}
		f.addChat(chat)
		provider := f.server()
		defer provider.Close()
		client := NewClient(provider.URL, testToken, 1000)
		bounds := DefaultContextBounds()
		complete, err := client.CaptureContext(t.Context(), "signal", chat.ID, nil, bounds)
		require.NoError(err)
		require.True(complete.Complete, complete.Reason)
		encoded, err := json.Marshal(complete)
		require.NoError(err)
		require.Greater(len(encoded), 1)
		bounds.MaxBytes = len(encoded)
		fits, err := client.CaptureContext(t.Context(), "signal", chat.ID, nil, bounds)
		require.NoError(err)
		require.True(fits.Complete, fits.Reason)
		encoded, err = json.Marshal(fits)
		require.NoError(err)
		assert.LessOrEqual(len(encoded), bounds.MaxBytes)
		bounds.MaxBytes--
		held, err := client.CaptureContext(t.Context(), "signal", chat.ID, nil, bounds)
		require.NoError(err)
		assert.False(held.Complete, "a message-only byte bound must not admit an oversized packet")
		assert.Equal("context_byte_limit", held.Reason)
		encoded, err = json.Marshal(held)
		require.NoError(err)
		assert.LessOrEqual(len(encoded), bounds.MaxBytes, "held output also respects the packet bound")
		bounds.MaxBytes = 1
		tiny, err := client.CaptureContext(t.Context(), "signal", chat.ID, nil, bounds)
		require.Error(err, "a cap smaller than result metadata cannot emit an oversized packet")
		assert.False(tiny.Complete)
		assert.Empty(tiny.Messages)
	})
}

func TestBeeperContextReplyClosure(t *testing.T) {
	for _, mode := range []string{"complete", "overflow", "missing", "cycle"} {
		t.Run(mode, func(t *testing.T) {
			assert := Assert.New(t)
			require := require.New(t)
			f := newFakeBeeper(t)
			base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
			ch := &fakeChat{ID: "!reply:example.test", AccountID: "signal", Type: "single", LastActivity: base.Add(time.Hour)}
			ch.Msgs = []fakeMsg{
				{ID: "parent", SortKey: 1, Timestamp: base, Text: "Original question"},
				{ID: "middle", SortKey: 2, Timestamp: base.Add(time.Second), Text: "Follow-up", LinkedMessageID: "parent"},
				{ID: "head", SortKey: 3, Timestamp: base.Add(2 * time.Second), Text: "Latest reply", LinkedMessageID: "middle"},
			}
			bounds := DefaultContextBounds()
			bounds.InitialMessages = 1
			bounds.MaxEvents = 3
			if mode == "overflow" {
				bounds.MaxEvents = 2
			}
			if mode == "missing" {
				ch.Msgs[1].LinkedMessageID = "not-visible"
			}
			if mode == "cycle" {
				ch.Msgs[0].LinkedMessageID = "head"
			}
			f.addChat(ch)
			server := f.server()
			defer server.Close()
			packet, err := NewClient(server.URL, testToken, 1000).CaptureContext(t.Context(), "signal", ch.ID, nil, bounds)
			require.NoError(err)
			if mode == "complete" {
				require.True(packet.Complete, packet.Reason)
				assert.Len(packet.Messages, 3)
				assert.True(packet.OlderHistoryOmitted)
			} else {
				assert.False(packet.Complete)
				assert.Contains(packet.Reason, "context_parent_")
			}
		})
	}
}

func TestBeeperContextTLSRequestBudget(t *testing.T) {
	assert := Assert.New(t)
	requirements := require.New(t)
	f := newFakeBeeper(t)
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	f.addChat(&fakeChat{ID: "!tls:example.test", AccountID: "signal", Type: "single", LastActivity: base,
		Msgs: []fakeMsg{{ID: "head", SortKey: 1, Timestamp: base, Text: "Synthetic TLS context"}}})
	handler := f.handler()
	var usedHTTP2 atomic.Bool
	var physicalRequests, posts atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 {
			usedHTTP2.Store(true)
		}
		if r.URL.Path == "/protocol" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		physicalRequests.Add(1)
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/archive") {
			posts.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client := NewClient(server.URL, testToken, 1000)
	client.http = server.Client()
	transport, ok := client.http.Transport.(*http.Transport)
	requirements.True(ok, "TLS fixture must expose its transport")
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	transport.Protocols.SetHTTP2(true)
	probe, err := client.http.Get(server.URL + "/protocol")
	requirements.NoError(err)
	assert.Equal(2, probe.ProtoMajor, "fixture must actually negotiate HTTP/2")
	requirements.NoError(probe.Body.Close())
	usedHTTP2.Store(false)
	packet, err := client.CaptureContext(t.Context(), "signal", "!tls:example.test", nil, DefaultContextBounds())
	requirements.NoError(err)
	requirements.True(packet.Complete, packet.Reason)
	assert.False(usedHTTP2.Load(), "context must avoid hidden HTTP/2 stream retries")
	assert.True(transport.Protocols.HTTP2(), "private capture must preserve the caller's transport")

	for _, limit := range []int{18, 19} {
		t.Run(fmt.Sprintf("shared-lifecycle-cap-%d", limit), func(t *testing.T) {
			assert := Assert.New(t)
			require := require.New(t)
			physicalRequests.Store(0)
			posts.Store(0)
			usedHTTP2.Store(false)
			scoped, closeScope, err := client.ForBoundedOperations(limit)
			require.NoError(err)
			defer closeScope()
			bounds := DefaultContextBounds()
			for range 2 { // Initial capture C and same-scope postmodel V.
				packet, err := scoped.CaptureContext(t.Context(), "signal", "!tls:example.test", nil, bounds)
				require.NoError(err)
				require.True(packet.Complete, packet.Reason)
				assert.Equal(5, packet.Requests, "captures report their own actual attempts")
			}
			_, err = scoped.GetChat(t.Context(), "!tls:example.test") // P
			require.NoError(err)
			bounds.MaxRequests = limit - scoped.Requests() - 3                                            // Reserve F/W/B.
			packet, err := scoped.CaptureContext(t.Context(), "signal", "!tls:example.test", nil, bounds) // A
			require.NoError(err)
			if limit == 18 {
				assert.False(packet.Complete)
				assert.Zero(posts.Load())
			} else {
				require.True(packet.Complete, packet.Reason)
				_, err = scoped.GetChat(t.Context(), "!tls:example.test") // F
				require.NoError(err)
				require.NoError(scoped.ArchiveChat(t.Context(), "!tls:example.test", true)) // W
				_, err = scoped.GetChat(t.Context(), "!tls:example.test")                   // B
				require.NoError(err)
				assert.Equal(int64(19), physicalRequests.Load())
				assert.Equal(int64(1), posts.Load())
				_, err = scoped.GetChat(t.Context(), "!tls:example.test")
				require.Error(err, "exhausted scope refuses before another HTTP attempt")
				assert.Equal(int64(19), physicalRequests.Load())
			}
			assert.LessOrEqual(physicalRequests.Load(), int64(limit))
			assert.Equal(int(physicalRequests.Load()), scoped.Requests())
			assert.False(usedHTTP2.Load(), "all lifecycle operations stay on private H1")
			assert.True(transport.Protocols.HTTP2(), "caller transport remains H2-capable")
		})
	}
}

// Bounded morning create owns a stricter transport than existing manual drafts.
func TestBeeperBoundedDraftOneShot(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			assert := Assert.New(t)
			require := require.New(t)
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(http.MethodPatch, r.Method)
				if status == http.StatusTemporaryRedirect || status == http.StatusPermanentRedirect {
					w.Header().Set("Location", "/must-not-replay")
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			original := NewClient(server.URL, testToken, 1000)
			scoped, closeScope, err := original.ForBoundedOperations(2)
			require.NoError(err)
			defer closeScope()
			text := "Synthetic bounded draft"
			_, err = scoped.SetDraft(t.Context(), "synthetic-chat", &text)
			require.Error(err)
			assert.Equal(int64(1), requests.Load())
			assert.Equal(1, scoped.Requests())
			assert.Nil(original.http.Transport, "manual/importer transport remains unchanged")
		})
	}
}
