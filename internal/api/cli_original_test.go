package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type cliOriginalFixture struct {
	srv       *Server
	st        *store.Store
	withRaw   int64
	noRaw     int64
	sourceID  int64
	rawBytes  []byte
	threadKey string
}

func newCLIOriginalFixture(t *testing.T, configure ...func(*ServerOptions)) *cliOriginalFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })
	options := ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Engine: engine,
		Logger: testLogger(),
	}
	for _, apply := range configure {
		apply(&options)
	}
	srv := NewServerWithOptions(options)
	src, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(t, err)
	convID, err := st.EnsureConversation(src.ID, "thread-original", "Original")
	require.NoError(t, err)
	raw := []byte("From: owner@example.com\r\nSubject: Original\r\n\r\nbody \xe9\n")
	persist := func(sourceMessageID string, sentAt time.Time, rawMIME []byte) int64 {
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message: &store.Message{
				SourceID:        src.ID,
				ConversationID:  convID,
				SourceMessageID: sourceMessageID,
				MessageType:     "email",
				SentAt:          sql.NullTime{Time: sentAt, Valid: true},
			},
			RawMIME: rawMIME,
		})
		require.NoError(t, err)
		return id
	}
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	withRaw := persist("provider-original", base, raw)
	noRaw := persist("provider-no-raw", base.Add(time.Hour), nil)
	return &cliOriginalFixture{srv: srv, st: st, withRaw: withRaw, noRaw: noRaw, sourceID: src.ID, rawBytes: raw, threadKey: "thread-original"}
}

func (f *cliOriginalFixture) get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	f.srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func requireErrorCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	require.Equal(t, status, w.Code, "status for body %s", w.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, code, resp.Error)
}

func TestHandleCLIMessageOriginal(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	t.Parallel()
	f := newCLIOriginalFixture(t)

	for _, target := range []string{
		"/api/v1/cli/message/original?id=" + strconv.FormatInt(f.withRaw, 10),
		"/api/v1/cli/message/original?source_message_id=provider-original&account=owner@example.com",
	} {
		w := f.get(t, target)
		must.Equal(http.StatusOK, w.Code, "%s: %s", target, w.Body.String())
		var resp cliOriginalMessageResponse
		must.NoError(json.NewDecoder(w.Body).Decode(&resp))
		checks.Equal(f.rawBytes, resp.MIME)
		checks.Equal(f.withRaw, resp.Message.MessageID)
		checks.Equal("provider-original", resp.Message.SourceMessageID)
		checks.Equal("thread-original", resp.Message.SourceConversationID)
		checks.Equal("owner@example.com", resp.Message.Account)
		checks.Equal(f.sourceID, resp.Message.SourceID)
	}

	requireErrorCode(t, f.get(t, "/api/v1/cli/message/original"), http.StatusBadRequest, "invalid_request")
	requireErrorCode(t, f.get(t, "/api/v1/cli/message/original?id=abc"), http.StatusBadRequest, "invalid_request")
	requireErrorCode(t, f.get(t, "/api/v1/cli/message/original?id=999999"), http.StatusNotFound, "message_not_found")
	requireErrorCode(t, f.get(t, "/api/v1/cli/message/original?id="+strconv.FormatInt(f.noRaw, 10)),
		http.StatusNotFound, "original_mime_unavailable")

	other, err := f.st.GetOrCreateSource("gmail", "second@example.com")
	must.NoError(err)
	otherConv, err := f.st.EnsureConversation(other.ID, "thread-original", "Other")
	must.NoError(err)
	_, err = f.st.PersistMessage(&store.MessagePersistData{Message: &store.Message{
		SourceID: other.ID, ConversationID: otherConv, SourceMessageID: "provider-original", MessageType: "email",
	}, RawMIME: []byte("x")})
	must.NoError(err)
	w := f.get(t, "/api/v1/cli/message/original?source_message_id=provider-original")
	must.Equal(http.StatusConflict, w.Code)
	var ambiguous ErrorResponse
	must.NoError(json.NewDecoder(w.Body).Decode(&ambiguous))
	checks.Equal("message_ambiguous", ambiguous.Error)
	checks.Contains(ambiguous.Message, "owner@example.com")
	checks.Contains(ambiguous.Message, "second@example.com")
	checks.NotContains(ambiguous.Message, "pass account")
}

func TestHandleCLIMessageOriginalByteLimit(t *testing.T) {
	must := require.New(t)
	f := newCLIOriginalFixture(t)
	target := "/api/v1/cli/message/original?id=" + strconv.FormatInt(f.withRaw, 10)
	requireErrorCode(t, f.get(t, target+"&max_bytes=1"), http.StatusRequestEntityTooLarge, "original_message_too_large")
	w := f.get(t, target+"&max_bytes="+strconv.Itoa(len(f.rawBytes)))
	must.Equal(http.StatusOK, w.Code, w.Body.String())
	var result cliOriginalMessageResponse
	must.NoError(json.NewDecoder(w.Body).Decode(&result))
	assert.Equal(t, f.rawBytes, result.MIME)
	for _, limit := range []string{"0", "-1", "9223372036854775807", "9223372036854775808", "bad"} {
		requireErrorCode(t, f.get(t, target+"&max_bytes="+limit), http.StatusBadRequest, "invalid_request")
	}
}

func TestHandleCLIMessageOriginalUnsupportedBackend(t *testing.T) {
	st := testutil.NewTestStore(t)
	engine, err := query.NewDuckDBEngine("", "", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Engine: engine,
		Logger: testLogger(),
	})
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/cli/message/original?id=1", nil))

	requireErrorCode(t, w, http.StatusServiceUnavailable, "original_export_unavailable")
}

func TestHandleCLIMessageThread(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	t.Parallel()
	f := newCLIOriginalFixture(t)

	w := f.get(t, "/api/v1/cli/message/thread?id="+strconv.FormatInt(f.noRaw, 10))
	must.Equal(http.StatusOK, w.Code, w.Body.String())
	var page struct {
		MessageID int64 `json:"message_id"`
		Total     int64 `json:"total"`
		HasMore   bool  `json:"has_more"`
		Messages  []struct {
			ID     int64 `json:"id"`
			HasRaw bool  `json:"has_raw"`
		} `json:"messages"`
	}
	must.NoError(json.NewDecoder(w.Body).Decode(&page))
	must.Len(page.Messages, 2)
	checks.Equal(f.withRaw, page.Messages[0].ID)
	checks.True(page.Messages[0].HasRaw)
	checks.Equal(f.noRaw, page.Messages[1].ID)
	checks.False(page.Messages[1].HasRaw)
	checks.Equal(int64(2), page.Total)
	checks.Equal(f.noRaw, page.MessageID)

	w = f.get(t, "/api/v1/cli/message/thread?thread_id=thread-original&account=owner@example.com&limit=1&offset=1")
	must.Equal(http.StatusOK, w.Code, w.Body.String())
	must.NoError(json.NewDecoder(w.Body).Decode(&page))
	must.Len(page.Messages, 1)
	checks.Equal(f.noRaw, page.Messages[0].ID)
	checks.False(page.HasMore)

	requireErrorCode(t, f.get(t, "/api/v1/cli/message/thread"), http.StatusBadRequest, "invalid_request")
	requireErrorCode(t, f.get(t, "/api/v1/cli/message/thread?thread_id=x&limit=501"), http.StatusBadRequest, "invalid_request")
	requireErrorCode(t, f.get(t, "/api/v1/cli/message/thread?thread_id=x&offset=-1"), http.StatusBadRequest, "invalid_request")
	requireErrorCode(t, f.get(t, "/api/v1/cli/message/thread?thread_id=missing"), http.StatusNotFound, "thread_not_found")
	requireErrorCode(t, f.get(t, "/api/v1/cli/message/thread?id=999999"), http.StatusNotFound, "message_not_found")
}

func TestHandleCLIMessageThreadAll(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	t.Parallel()
	f := newCLIOriginalFixture(t)
	convID, err := f.st.EnsureConversation(f.sourceID, f.threadKey, "Original")
	must.NoError(err)
	for i := range 99 {
		_, err := f.st.UpsertMessage(&store.Message{
			SourceID: f.sourceID, ConversationID: convID,
			SourceMessageID: "additional-" + strconv.Itoa(i), MessageType: "email",
		})
		must.NoError(err)
	}

	w := f.get(t, "/api/v1/cli/message/thread?thread_id=thread-original&all=true")
	must.Equal(http.StatusOK, w.Code, w.Body.String())
	var page struct {
		Total    int64             `json:"total"`
		Offset   int               `json:"offset"`
		HasMore  bool              `json:"has_more"`
		Messages []json.RawMessage `json:"messages"`
	}
	must.NoError(json.NewDecoder(w.Body).Decode(&page))
	checks.Len(page.Messages, 101)
	checks.Equal(int64(101), page.Total)
	checks.Zero(page.Offset)
	checks.False(page.HasMore)

	for _, suffix := range []string{"&limit=1", "&offset=0", "&limit=", "&offset="} {
		requireErrorCode(t, f.get(t, "/api/v1/cli/message/thread?thread_id=thread-original&all=true"+suffix),
			http.StatusBadRequest, "invalid_request")
	}
	requireErrorCode(t, f.get(t, "/api/v1/cli/message/thread?thread_id=thread-original&all=invalid"),
		http.StatusBadRequest, "invalid_request")
}

func TestHandleCLIMessageRawVisibility(t *testing.T) {
	must := require.New(t)
	f := newCLIOriginalFixture(t)
	other, err := f.st.GetOrCreateSource("gmail", "other@example.com")
	must.NoError(err)
	convID, err := f.st.EnsureConversation(other.ID, "other-thread", "Other")
	must.NoError(err)
	_, err = f.st.UpsertMessage(&store.Message{SourceID: other.ID, ConversationID: convID, SourceMessageID: "provider-original", MessageType: "email"})
	must.NoError(err)
	requireErrorCode(t, f.get(t, "/api/v1/cli/message/raw?id=provider-original"), http.StatusConflict, "message_ambiguous")
}
