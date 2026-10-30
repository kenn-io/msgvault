package sync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func gmailCoverageEnv(t *testing.T) *TestEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "durable.db"))
	require.NoError(t, err)
	require.NoError(t, st.InitSchema())
	mock := gmail.NewMockAPI()
	mock.Profile = &gmail.Profile{EmailAddress: testEmail, HistoryID: 1000}
	e := &TestEnv{Store: st, Mock: mock, Syncer: New(mock, st, nil), TmpDir: dir, Context: context.Background()}
	t.Cleanup(func() { require.NoError(t, e.Store.Close()) })
	return e
}
func gmailCoverageReopen(t *testing.T, e *TestEnv) {
	t.Helper()
	api := e.Syncer.client
	require.NoError(t, e.Store.Close())
	st, err := store.Open(filepath.Join(e.TmpDir, "durable.db"))
	require.NoError(t, err)
	e.Store = st
	e.Syncer = New(api, st, nil)
}
func gmailCoverageSource(t *testing.T, e *TestEnv) *store.Source {
	t.Helper()
	s, err := e.Store.GetSourceByIdentifier(testEmail)
	require.NoError(t, err)
	return s
}
func gmailCoverageAbsent(t *testing.T, e *TestEnv, id string) {
	t.Helper()
	s := gmailCoverageSource(t, e)
	m, err := e.Store.MessageExistsBatch(s.ID, []string{id})
	require.NoError(t, err)
	require.Empty(t, m)
	var raws int
	require.NoError(t, e.Store.DB().QueryRow("SELECT count(*) FROM message_raw").Scan(&raws))
	t.Logf("missing=%s total_raws=%d cursor=%s", id, raws, s.SyncCursor.String)
}
func gmailCoverageRaw(t *testing.T, e *TestEnv, id string) {
	t.Helper()
	s := gmailCoverageSource(t, e)
	m, err := e.Store.MessageExistsBatch(s.ID, []string{id})
	require.NoError(t, err)
	require.Contains(t, m, id)
	raw, err := e.Store.GetMessageRaw(m[id])
	require.NoError(t, err)
	require.Equal(t, testMIME(), raw)
}

type gmailCoverageTransport struct {
	mu          sync.Mutex
	requests    []string
	history     uint64
	event       string
	expire      bool
	failPage    bool
	failGet     bool
	omitSpecial bool
}

func (f *gmailCoverageTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Host != "gmail.googleapis.com" || r.Method != http.MethodGet {
		return nil, fmt.Errorf("unexpected synthetic request: %s %s", r.Method, r.URL)
	}
	f.requests = append(f.requests, r.URL.RequestURI())
	p := r.URL.Path
	q := r.URL.Query()
	status := 200
	var body any
	switch {
	case strings.HasSuffix(p, "/profile"):
		body = map[string]any{"emailAddress": testEmail, "messagesTotal": 4, "historyId": strconv.FormatUint(f.history, 10)}
	case strings.HasSuffix(p, "/labels"):
		body = map[string]any{"labels": []map[string]string{{"id": "INBOX", "name": "INBOX", "type": "system"}, {"id": "SPAM", "name": "SPAM", "type": "system"}, {"id": "TRASH", "name": "TRASH", "type": "system"}}}
	case strings.HasSuffix(p, "/history"):
		if f.expire {
			status = 404
			body = map[string]any{"error": map[string]any{"code": 404, "message": "synthetic expired"}}
			f.expire = false
		} else {
			h := []any{}
			if f.event != "" {
				h = append(h, map[string]any{"id": strconv.FormatUint(f.history, 10), "messagesAdded": []any{map[string]any{"message": map[string]any{"id": f.event, "threadId": "thread_" + f.event}}}})
			}
			body = map[string]any{"historyId": strconv.FormatUint(f.history, 10), "history": h}
		}
	case strings.HasSuffix(p, "/messages"):
		if f.failPage && q.Get("pageToken") == "p2" {
			status = 400
			body = map[string]any{"error": map[string]any{"code": 400, "message": "synthetic page failure"}}
			f.failPage = false
			break
		}
		ids := []string{"normal1", "normal2"}
		if q.Get("includeSpamTrash") == "true" && !f.omitSpecial && !strings.Contains(q.Get("q"), "-in:spam") {
			ids = append(ids, "spam", "trash")
		}
		page := 0
		if q.Get("pageToken") == "p2" {
			page = 1
		}
		half := len(ids) / 2
		chosen := ids[:half]
		if page == 1 {
			chosen = ids[half:]
		}
		messages := []any{}
		for _, id := range chosen {
			messages = append(messages, map[string]any{"id": id, "threadId": "thread_" + id})
		}
		resp := map[string]any{"messages": messages, "resultSizeEstimate": len(ids)}
		if page == 0 {
			resp["nextPageToken"] = "p2"
		}
		body = resp
	case strings.Contains(p, "/messages/"):
		id := p[strings.LastIndex(p, "/")+1:]
		if q.Get("format") != "raw" {
			return nil, fmt.Errorf("unexpected format: %s", r.URL)
		}
		if f.failGet && id == "spam" {
			status = 400
			body = map[string]any{"error": map[string]any{"code": 400, "message": "synthetic transient fetch"}}
			break
		}
		label := "INBOX"
		if id == "spam" {
			label = "SPAM"
		}
		if id == "trash" {
			label = "TRASH"
		}
		body = map[string]any{"id": id, "threadId": "thread_" + id, "labelIds": []string{label}, "historyId": strconv.FormatUint(f.history, 10), "internalDate": "1700000000000", "raw": base64.RawURLEncoding.EncodeToString(testMIME())}
	default:
		return nil, fmt.Errorf("unexpected synthetic endpoint: %s", r.URL)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(b))), Request: r}, nil
}
func TestGmailCoverageScenario3(t *testing.T) {
	require := require.New(t)

	e := gmailCoverageEnv(t)
	f := &gmailCoverageTransport{history: 1000, failPage: true}
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: f}
	t.Cleanup(func() { http.DefaultClient = old })
	// nil source uses DefaultClient and does not construct any OAuth credential.
	e.Syncer = New(gmail.NewClient(nil, gmail.WithConcurrency(1)), e.Store, nil)
	first, err := e.Syncer.Full(e.Context, testEmail)
	require.Error(err)
	require.Nil(first)
	t.Log("synthetic page 2 failure saved resume checkpoint")
	gmailCoverageReopen(t, e)
	resumed, err := e.Syncer.Full(e.Context, testEmail)
	require.NoError(err)
	require.True(resumed.WasResumed)
	require.Equal("p2", resumed.ResumedFromToken)
	gmailCoverageRaw(t, e, "normal1")
	gmailCoverageRaw(t, e, "normal2")
	gmailCoverageRaw(t, e, "spam")
	gmailCoverageRaw(t, e, "trash")
	_, err = e.Syncer.Incremental(e.Context, gmailCoverageSource(t, e))
	require.NoError(err)
	gmailCoverageRaw(t, e, "spam")
	// Expired history drives real full + complete snapshot + catchup paths.
	f.history = 1001
	f.expire = true
	recovered, err := e.Syncer.IncrementalWithHistoryRecovery(e.Context, gmailCoverageSource(t, e), nil)
	require.NoError(err)
	require.Zero(recovered.Errors)
	gmailCoverageReopen(t, e)
	gmailCoverageRaw(t, e, "spam")
	gmailCoverageRaw(t, e, "trash")
	inventory := 0
	ordinary := 0
	for _, request := range f.requests {
		t.Log("in-memory HTTP", request)
		if strings.Contains(request, "/messages?") {
			if strings.Contains(request, "includeSpamTrash=true") {
				inventory++
			} else {
				ordinary++
			}
		}
	}
	require.Positive(inventory, "acquisition, resume and reconciliation must list messages")
	require.Zero(ordinary)
}
