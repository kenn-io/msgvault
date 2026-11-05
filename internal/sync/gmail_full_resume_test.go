package sync

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// The continuation lists only the old tail. Arrivals are visible on a fresh
// first page and in History strictly after their event ID, as on Gmail.
type fullResumeTransport struct {
	gmailCoverageTransport

	stopAt        string
	arrivals      map[string]uint64
	listTokens    []string
	historyStarts []uint64
}

func (f *fullResumeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p, q := r.URL.Path, r.URL.Query()
	var body any
	status := http.StatusOK
	switch {
	case strings.HasSuffix(p, "/messages"):
		token := q.Get("pageToken")
		f.listTokens = append(f.listTokens, token)
		if token == f.stopAt && f.stopAt != "" {
			status = http.StatusBadRequest // Fail without HTTP retry/backoff.
			body = map[string]any{"error": map[string]any{"code": status, "message": "synthetic interrupted listing"}}
			break
		}
		ids, next := []string{"normal1"}, "p2"
		switch token {
		case "":
			for id := range f.arrivals {
				ids = append(ids, id)
			}
		case "p2":
			ids, next = []string{"normal2"}, "p3"
		case "p3":
			ids, next = []string{"normal3"}, ""
		default:
			return nil, fmt.Errorf("unexpected listing token %q", token)
		}
		messages := []any{}
		for _, id := range ids {
			messages = append(messages, map[string]any{"id": id, "threadId": "thread_" + id})
		}
		body = map[string]any{"messages": messages, "nextPageToken": next}
	case strings.HasSuffix(p, "/history"):
		start, err := strconv.ParseUint(q.Get("startHistoryId"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse synthetic History start: %w", err)
		}
		f.historyStarts = append(f.historyStarts, start)
		if f.expire {
			return f.gmailCoverageTransport.RoundTrip(r)
		}
		events := []any{}
		for id, at := range f.arrivals {
			if at > start {
				events = append(events, map[string]any{"id": strconv.FormatUint(at, 10), "messagesAdded": []any{map[string]any{"message": map[string]any{"id": id, "threadId": "thread_" + id}}}})
			}
		}
		body = map[string]any{"historyId": strconv.FormatUint(f.history, 10), "history": events}
	default:
		return f.gmailCoverageTransport.RoundTrip(r)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(encoded))), Request: r}, nil
}

func TestGmailFullResumeHistoryContinuity(t *testing.T) {
	// serial: the real Gmail client uses http.DefaultClient without OAuth.
	for _, scenario := range []struct {
		name          string
		interruptions int
		query         string
		limit         int
	}{
		{name: "once", interruptions: 1},
		{name: "twice", interruptions: 2},
		{name: "filtered", interruptions: 1, query: "in:inbox"},
		{name: "limited", interruptions: 1, limit: 2},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			interruptions := scenario.interruptions
			opts := DefaultOptions()
			opts.Query, opts.Limit = scenario.query, scenario.limit
			wantBoundary := "1000"
			wantHistoryStart := uint64(1000)
			if scenario.query != "" || scenario.limit > 0 {
				wantBoundary = "" // Partial full sync never publishes a source cursor.
				wantHistoryStart = 900
			}
			e := acquisitionDurabilityEnv(t)
			source := e.CreateSourceWithHistory(t, "900")
			f := &fullResumeTransport{history: 1000, arrivals: map[string]uint64{}}
			api := gmailTransportClient(t, f)
			e.Syncer = New(api, e.Store, opts)
			for attempt := range interruptions {
				f.stopAt = []string{"p2", "p3"}[attempt]
				_, err := e.Syncer.Full(t.Context(), testEmail)
				require.ErrorContains(err, "synthetic interrupted listing")
				run, err := e.Store.GetLatestSync(source.ID)
				require.NoError(err)
				require.Equal(f.stopAt, run.CursorBefore.String, "stop only after the preceding page is durable")
				require.Equal(store.SyncStatusFailed, run.Status)
				assert.Equal(wantBoundary, run.CursorAfter.String, "each checkpoint retains the first History boundary")
				require.Equal("900", gmailCoverageSource(t, e).SyncCursor.String, "interruption must not publish")
				reopenAcquisitionStore(t, e)
				e.Syncer = New(api, e.Store, opts)
				f.history += 100
				f.arrivals[fmt.Sprintf("arrival%d", attempt+1)] = f.history - 50
			}
			resumeToken := f.stopAt
			f.stopAt = ""
			f.listTokens = nil
			summary, err := e.Syncer.Full(t.Context(), testEmail)
			require.NoError(err)
			require.True(summary.WasResumed)
			require.Equal(resumeToken, summary.ResumedFromToken)
			assert.NotContains(f.listTokens, "", "exercise continuation, not a fresh scan")
			if scenario.limit > 0 {
				assert.Equal(int64(scenario.limit), summary.MessagesFound, "resume must retain the limit across runs")
			}
			reopenAcquisitionStore(t, e)
			e.Syncer = New(api, e.Store, nil) // The following incremental is unrestricted.
			assert.Equal(strconv.FormatUint(wantHistoryStart, 10), gmailCoverageSource(t, e).SyncCursor.String)
			for id, at := range f.arrivals {
				existing, err := e.Store.MessageExistsBatch(source.ID, []string{id})
				require.NoError(err)
				require.Empty(existing, "arrival must be absent from the saved listing tail")
				t.Logf("before incremental: absent=%s event=%d published=%s", id, at, gmailCoverageSource(t, e).SyncCursor.String)
			}
			_, err = e.Syncer.Incremental(t.Context(), gmailCoverageSource(t, e))
			require.NoError(err)
			reopenAcquisitionStore(t, e)
			for id := range f.arrivals {
				assertAcquisitionPresent(t, e, id)
			}
			assert.Equal([]uint64{wantHistoryStart}, f.historyStarts, "History must cover the gap across every interruption")
			assert.Equal(strconv.FormatUint(f.history, 10), gmailCoverageSource(t, e).SyncCursor.String)
		})
	}
}

func TestGmailFullResumeRestartsWithoutSafeBoundary(t *testing.T) {
	// serial: gmailTransportClient replaces http.DefaultClient.
	for _, reason := range []string{"previous_release", "noresume"} {
		t.Run(reason, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			e := acquisitionDurabilityEnv(t)
			e.CreateSourceWithHistory(t, "900")
			f := &fullResumeTransport{history: 1000, stopAt: "p2", arrivals: map[string]uint64{}}
			api := gmailTransportClient(t, f)
			e.Syncer = New(api, e.Store, nil)
			_, err := e.Syncer.Full(t.Context(), testEmail)
			require.ErrorContains(err, "synthetic interrupted listing")
			opts := DefaultOptions()
			if reason == "noresume" {
				opts.NoResume = true
			} else {
				// Model the same request's fingerprint from the previous release.
				fingerprint := fmt.Sprintf("full:v2-spam-trash:%x", sha256.Sum256([]byte("full:v1\x00\x00\x000\x00false")))
				_, err := e.Store.DB().Exec(e.Store.Rebind("UPDATE sync_runs SET request_fingerprint = ?"), fingerprint)
				require.NoError(err)
			}
			reopenAcquisitionStore(t, e)
			e.Syncer = New(api, e.Store, opts)
			f.history, f.stopAt = 1100, ""
			f.arrivals["arrival1"] = 1050
			f.listTokens = nil
			summary, err := e.Syncer.Full(t.Context(), testEmail)
			require.NoError(err)
			assert.False(summary.WasResumed)
			assert.Empty(summary.ResumedFromToken)
			assert.Equal([]string{"", "p2", "p3"}, f.listTokens)
			assert.Equal(uint64(1100), summary.FinalHistoryID, "a fresh listing can use a fresh boundary")
			assertAcquisitionPresent(t, e, "normal1", "normal2", "normal3", "arrival1")
		})
	}
}

func TestGmailFullResumeRequiresDurableBoundary(t *testing.T) {
	// serial: gmailTransportClient replaces http.DefaultClient.
	require := require.New(t)
	assert := assert.New(t)

	e := acquisitionDurabilityEnv(t)
	e.CreateSourceWithHistory(t, "900")
	f := &fullResumeTransport{history: 1000}
	api := gmailTransportClient(t, f)
	e.Syncer = New(api, e.Store, nil)
	allow := acquisitionWriteFault(t, e, "sync_runs", "UPDATE", "NEW.cursor_after = '1000'")
	_, err := e.Syncer.Full(t.Context(), testEmail)
	require.ErrorContains(err, "synthetic acquisition write failure")
	assert.Empty(f.listTokens, "stop before listing when the boundary cannot be saved")
	assert.Equal("900", gmailCoverageSource(t, e).SyncCursor.String)
	allow()
	reopenAcquisitionStore(t, e)
	e.Syncer = New(api, e.Store, nil)
	summary, err := e.Syncer.Full(t.Context(), testEmail)
	require.NoError(err)
	assert.Equal(uint64(1000), summary.FinalHistoryID)
	assert.False(summary.WasResumed)
	assertAcquisitionPresent(t, e, "normal1", "normal2", "normal3")
}

func TestGmailFullResumeHistoryExpiration(t *testing.T) {
	// serial: gmailTransportClient replaces http.DefaultClient.
	for _, phase := range []string{"interrupted", "completed"} {
		t.Run(phase, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			e := acquisitionDurabilityEnv(t)
			e.CreateSourceWithHistory(t, "900")
			f := &fullResumeTransport{history: 1000, stopAt: "p2", arrivals: map[string]uint64{}}
			api := gmailTransportClient(t, f)
			e.Syncer = New(api, e.Store, nil)
			_, err := e.Syncer.Full(t.Context(), testEmail)
			require.ErrorContains(err, "synthetic interrupted listing")
			reopenAcquisitionStore(t, e)
			e.Syncer = New(api, e.Store, nil)
			f.history, f.stopAt = 1100, ""
			f.arrivals["arrival1"] = 1050
			wantStart := uint64(900)
			if phase == "completed" {
				_, err = e.Syncer.Full(t.Context(), testEmail)
				require.NoError(err)
				wantStart = 1000
			}
			f.listTokens, f.expire = nil, true
			summary, err := e.Syncer.IncrementalWithHistoryRecovery(t.Context(), gmailCoverageSource(t, e), nil)
			require.NoError(err)
			assert.Equal([]uint64{wantStart}, f.historyStarts, "exercise an expired source cursor")
			require.NotEmpty(f.listTokens)
			assert.Empty(f.listTokens[0], "History recovery cannot reuse an ordinary full checkpoint")
			assert.Equal(uint64(1100), summary.FinalHistoryID)
			reopenAcquisitionStore(t, e)
			assert.Equal("1100", gmailCoverageSource(t, e).SyncCursor.String)
			assertAcquisitionPresent(t, e, "normal1", "normal2", "normal3", "arrival1")
		})
	}
}
