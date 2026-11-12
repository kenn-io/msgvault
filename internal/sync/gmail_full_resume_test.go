package sync

import (
	"fmt"
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
	return gmailJSONResponse(r, status, body)
}

func TestGmailFullResumeHistoryContinuity(t *testing.T) {
	// serial: the real Gmail client uses http.DefaultClient without OAuth.
	for _, scenario := range []struct {
		name               string
		interruptions      int
		query              string
		limit              int
		incrementalBetween bool
	}{
		{name: "once", interruptions: 1},
		{name: "twice", interruptions: 2},
		{name: "incrementalBetween", interruptions: 1, incrementalBetween: true},
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
			if scenario.query != "" || scenario.limit > 0 {
				wantBoundary = "" // Partial full sync doesn't pin a History boundary.
			}
			e := acquisitionDurabilityEnv(t)
			source := e.CreateSourceWithHistory(t, "900")
			f := &fullResumeTransport{history: 1000, arrivals: map[string]uint64{}}
			api := gmailTransportClient(t, f)
			e.Syncer = New(api, e.Store, opts)
			for attempt := range interruptions {
				f.stopAt = fmt.Sprintf("p%d", attempt+2)
				_, err := e.Syncer.Full(t.Context(), testEmail)
				require.ErrorContains(err, "synthetic interrupted listing")
				run, err := e.Store.GetLatestSync(source.ID)
				require.NoError(err)
				require.Equal(f.stopAt, run.CursorBefore.String, "stop only after the preceding page is durable")
				require.Equal(store.SyncStatusFailed, run.Status)
				assert.Equal(wantBoundary, run.CursorAfter.String, "each checkpoint retains the first History boundary")
				if wantBoundary == "" {
					return
				}
				require.Equal("900", gmailCoverageSource(t.Context(), t, e).SyncCursor.String, "interruption must not publish")
				reopenAcquisitionStore(t, e)
				e.Syncer = New(api, e.Store, opts)
				f.history += 100
				f.arrivals[fmt.Sprintf("arrival%d", attempt+1)] = f.history - 50
			}
			wantHistoryStart := uint64(1000)
			wantHistoryStarts := []uint64{1000}
			if scenario.incrementalBetween {
				_, err := e.Syncer.Incremental(t.Context(), gmailCoverageSource(t.Context(), t, e))
				require.NoError(err)
				require.Equal("1100", gmailCoverageSource(t.Context(), t, e).SyncCursor.String)
				reopenAcquisitionStore(t, e)
				e.Syncer = New(api, e.Store, opts)
				wantHistoryStart = 1100
				wantHistoryStarts = []uint64{900}
			}
			resumeToken := f.stopAt
			f.stopAt = ""
			f.listTokens = nil
			summary, err := e.Syncer.Full(t.Context(), testEmail)
			require.NoError(err)
			require.True(summary.WasResumed)
			require.Equal(resumeToken, summary.ResumedFromToken)
			assert.Equal(wantHistoryStart, summary.FinalHistoryID)
			assert.NotContains(f.listTokens, "", "exercise continuation, not a fresh scan")
			reopenAcquisitionStore(t, e)
			e.Syncer = New(api, e.Store, nil) // The following incremental is unrestricted.
			assert.Equal(strconv.FormatUint(wantHistoryStart, 10), gmailCoverageSource(t.Context(), t, e).SyncCursor.String)
			_, err = e.Syncer.Incremental(t.Context(), gmailCoverageSource(t.Context(), t, e))
			require.NoError(err)
			reopenAcquisitionStore(t, e)
			for id := range f.arrivals {
				assertAcquisitionPresent(t, e, id)
			}
			assert.Equal(wantHistoryStarts, f.historyStarts, "History must cover the gap across every interruption")
			assert.Equal(strconv.FormatUint(f.history, 10), gmailCoverageSource(t.Context(), t, e).SyncCursor.String)
		})
	}
}

func TestGmailFullResumeHistoryExpiration(t *testing.T) {
	// serial: gmailTransportClient replaces http.DefaultClient.
	require := require.New(t)
	assert := assert.New(t)

	e := acquisitionDurabilityEnv(t)
	e.CreateSourceWithHistory(t, "900")
	f := &fullResumeTransport{history: 1000, stopAt: "p2"}
	api := gmailTransportClient(t, f)
	e.Syncer = New(api, e.Store, nil)
	_, err := e.Syncer.Full(t.Context(), testEmail)
	require.ErrorContains(err, "synthetic interrupted listing")
	reopenAcquisitionStore(t, e)
	e.Syncer = New(api, e.Store, nil)
	f.history, f.stopAt = 1100, ""
	f.listTokens, f.expire = nil, true
	summary, err := e.Syncer.IncrementalWithHistoryRecovery(t.Context(), gmailCoverageSource(t.Context(), t, e), nil)
	require.NoError(err)
	assert.False(summary.WasResumed)
	require.NotEmpty(f.listTokens)
	assert.Empty(f.listTokens[0], "History recovery cannot reuse an ordinary full checkpoint")
}
