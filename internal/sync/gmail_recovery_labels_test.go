package sync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

// Extend the HTTP fixture, not the API: recovery must use the real client's
// capabilities. Message reads can run concurrently; provider changes are made
// only between sync attempts.
type recoveryLabelsTransport struct {
	gmailCoverageTransport

	labels        map[string][]string
	changed       bool
	rawGets       int
	historyStarts []string
	fault         string
	gone          bool
	cancel        context.CancelFunc
}

func (f *recoveryLabelsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body any
	status := http.StatusOK
	switch {
	case strings.HasSuffix(r.URL.Path, "/history"):
		f.historyStarts = append(f.historyStarts, r.URL.Query().Get("startHistoryId"))
		return f.gmailCoverageTransport.RoundTrip(r)
	case strings.HasSuffix(r.URL.Path, "/messages"):
		if f.changed {
			// A change during enumeration forces an actual empty catch-up
			// from the boundary captured before the first page.
			f.history = 2001
		}
		if f.gone && r.URL.Query().Get("pageToken") == "p2" {
			body = map[string]any{"messages": []map[string]string{{"id": "spam", "threadId": "thread_spam"}}, "resultSizeEstimate": 3}
			break
		}
		return f.gmailCoverageTransport.RoundTrip(r)
	case strings.HasSuffix(r.URL.Path, "/labels"):
		custom := "Label_old"
		if f.changed {
			custom = "Label_new"
		}
		body = map[string]any{"labels": []map[string]string{
			{"id": "INBOX", "name": "INBOX", "type": "system"},
			{"id": "TRASH", "name": "TRASH", "type": "system"},
			{"id": "SPAM", "name": "SPAM", "type": "system"},
			{"id": custom, "name": custom, "type": "user"},
		}}
	case strings.Contains(r.URL.Path, "/messages/"):
		id := path.Base(r.URL.Path)
		body = map[string]any{"id": id, "labelIds": f.labels[id]}
		switch r.URL.Query().Get("format") {
		case "raw":
			f.mu.Lock()
			f.rawGets++
			f.mu.Unlock()
			body = map[string]any{"id": id, "threadId": "thread_" + id, "labelIds": f.labels[id],
				"historyId": "1000", "internalDate": "1700000000000",
				"raw": base64.RawURLEncoding.EncodeToString(testMIMEWithAttachment())}
		case "metadata":
			// An omitted labelIds field is Gmail's valid empty set.
			if len(f.labels[id]) == 0 {
				body = map[string]any{"id": id}
			}
			if id == "trash" {
				switch f.fault {
				case "gone":
					f.gone = true
					status = http.StatusNotFound
					body = map[string]any{"error": map[string]any{"code": status, "message": "synthetic message disappeared"}}
				case "read":
					status = http.StatusBadRequest // Non-retryable provider failure.
					body = map[string]any{"error": map[string]any{"code": status, "message": "synthetic label read failure"}}
				case "identity":
					body = map[string]any{"id": "different-message"}
				case "unknown":
					body = map[string]any{"id": id, "labelIds": append([]string{"Label_unknown"}, f.labels[id]...)}
				case "cancel":
					f.cancel()
					return nil, r.Context().Err()
				}
			}
		default:
			return nil, fmt.Errorf("unexpected message format: %s", r.URL)
		}
	default:
		return f.gmailCoverageTransport.RoundTrip(r)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode synthetic Gmail response: %w", err)
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(string(data))), Request: r}, nil
}

func recoveryLabelsEnv(t *testing.T) (*TestEnv, *recoveryLabelsTransport, map[string]int64) {
	t.Helper()
	require := require.New(t)
	// Serial: the real Gmail client uses http.DefaultClient without OAuth.
	e := acquisitionDurabilityEnv(t)
	f := &recoveryLabelsTransport{history: 1000, labels: map[string][]string{
		"normal1": {"INBOX"}, "normal2": {"INBOX", "Label_old"}, "spam": {"SPAM"}, "trash": {"TRASH"},
	}}
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: f}
	t.Cleanup(func() { http.DefaultClient = old })
	api := gmail.NewClient(nil, gmail.WithConcurrency(2))
	e.Syncer = New(api, e.Store, nil)
	_, err := e.Syncer.Full(t.Context(), testEmail)
	require.NoError(err)
	source := gmailCoverageSource(t, e)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"normal1", "normal2", "spam", "trash"})
	require.NoError(err)
	require.Len(ids, 4)
	reopenAcquisitionStore(t, e)
	e.Syncer = New(api, e.Store, nil)
	f.changed, f.expire, f.history = true, true, 2000
	f.labels = map[string][]string{
		"normal1": {"TRASH"}, "normal2": {"SPAM", "Label_new"}, "spam": {}, "trash": {"INBOX"},
	}
	return e, f, ids
}

func assertRecoveryLabels(t *testing.T, e *TestEnv, ids map[string]int64, labels map[string][]string) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)
	source := gmailCoverageSource(t, e)
	for id, want := range labels {
		actual, err := e.Store.MessageLabelIDsContext(t.Context(), ids[id])
		require.NoError(err)
		expected := make([]int64, 0, len(want))
		for _, label := range want {
			internalID, err := e.Store.LabelIDContext(t.Context(), source.ID, label)
			require.NoError(err)
			expected = append(expected, internalID)
		}
		assert.ElementsMatch(expected, actual, "current memberships for %s must replace the old set", id)
		raw, err := e.Store.GetMessageRaw(ids[id])
		require.NoError(err)
		assert.Equal(testMIMEWithAttachment(), raw, "label repair must preserve RAW for %s", id)
	}
}

func TestGmailRecoveryReplacesArchivedLabels(t *testing.T) {
	for _, fault := range []string{"", "unknown"} {
		t.Run(fault, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			e, f, ids := recoveryLabelsEnv(t)
			f.fault = fault
			summary, err := e.Syncer.IncrementalWithHistoryRecovery(t.Context(), gmailCoverageSource(t, e), nil)
			require.NoError(err)
			require.Zero(summary.Errors)
			assert.Equal(int64(4), summary.MessagesUpdated)
			assert.Equal(uint64(2001), summary.FinalHistoryID)
			assert.Equal([]string{"1000", "2000"}, f.historyStarts, "expire old History, then consume the empty catch-up")
			reopenAcquisitionStore(t, e)
			assertRecoveryLabels(t, e, ids, f.labels)
			assert.Equal(4, f.rawGets, "already archived originals must not be downloaded again")
			assert.Equal("2001", gmailCoverageSource(t, e).SyncCursor.String)
		})
	}
}

func TestGmailRecoveryLabelFailureRetainsCheckpoint(t *testing.T) {
	for _, fault := range []string{"read", "identity", "write", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			e, f, ids := recoveryLabelsEnv(t)
			api := e.Syncer.client
			removeFault := func() {}
			if fault == "write" {
				removeFault = acquisitionWriteFault(t, e, "message_labels", "INSERT", fmt.Sprintf("NEW.message_id = %d", ids["trash"]))
			} else {
				f.fault = fault
			}
			for range 2 {
				ctx, cancel := context.WithCancel(t.Context())
				f.cancel = cancel
				_, err := e.Syncer.IncrementalWithHistoryRecovery(ctx, gmailCoverageSource(t, e), nil)
				cancel()
				if fault == "cancel" {
					require.ErrorIs(err, context.Canceled)
				} else {
					require.ErrorContains(err, map[string]string{
						"read":     "synthetic label read failure",
						"identity": "unexpected message identity",
						"write":    "synthetic acquisition write failure",
					}[fault], "required metadata reconciliation cannot report success")
				}
				reopenAcquisitionStore(t, e)
				e.Syncer = New(api, e.Store, nil)
				assert.Equal("1000", gmailCoverageSource(t, e).SyncCursor.String)
				prior, err := e.Store.GetLatestCheckpointedSync(gmailCoverageSource(t, e).ID)
				require.NoError(err)
				assert.Equal("2000", prior.CursorAfter.String)
				cp, err := decodeRecoveryCheckpoint(prior.CursorBefore.String)
				require.NoError(err)
				assert.Equal(recoveryEnumerate, cp.Phase)
				assert.Equal("p2", cp.PageToken, "a failed page must be retried")
				assertRecoveryLabels(t, e, ids, map[string][]string{"trash": {"TRASH"}})
				debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), gmailCoverageSource(t, e).ID)
				require.NoError(err)
				assert.Empty(debt, "metadata failures must not trigger RAW acquisition replay")
			}
			removeFault()
			f.fault = ""
			summary, err := e.Syncer.IncrementalWithHistoryRecovery(t.Context(), gmailCoverageSource(t, e), nil)
			require.NoError(err)
			assert.Zero(summary.Errors)
			assert.Equal(int64(4), summary.MessagesUpdated)
			reopenAcquisitionStore(t, e)
			assertRecoveryLabels(t, e, ids, f.labels)
			assert.Equal(4, f.rawGets)
			assert.Equal("2001", gmailCoverageSource(t, e).SyncCursor.String)
		})
	}
}

func TestGmailRecoveryLegacyLabelCheckpointRestarts(t *testing.T) {
	for _, phase := range []string{recoveryEnumerate, recoveryReconcile, recoveryCatchup} {
		t.Run(phase, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			e, f, ids := recoveryLabelsEnv(t)
			api := e.Syncer.client
			source := gmailCoverageSource(t, e)
			run, err := e.Store.StartSync(source.ID, "full")
			require.NoError(err)
			_, err = e.Store.DB().Exec(e.Store.Rebind("UPDATE sync_runs SET request_fingerprint=? WHERE id=?"),
				store.GmailHistoryRecoveryRequestFingerprint+":spam-trash:v1", run)
			require.NoError(err)
			token := ""
			if phase == recoveryEnumerate {
				token = "p2"
			}
			state := &syncState{recoveryPhase: phase, handoffCursor: "1500",
				checkpoint: &store.Checkpoint{PageToken: token}}
			require.NoError(e.Store.ScopedToSync(source.ID, run).UpdateSyncCheckpoint(run, state.durableCheckpoint()))
			require.NoError(e.Store.FailSync(run, "synthetic old recovery interruption"))
			reopenAcquisitionStore(t, e)
			e.Syncer = New(api, e.Store, nil)
			f.expire = false
			_, err = e.Syncer.Full(t.Context(), testEmail)
			require.NoError(err)
			assert.Equal([]string{"1500"}, f.historyStarts, "restart listings, retaining the safe History boundary")
			assertRecoveryLabels(t, e, ids, f.labels)
			assert.Equal(4, f.rawGets)
		})
	}
}

func TestGmailRecoveryLabelMessageDisappears(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	e, f, ids := recoveryLabelsEnv(t)
	f.fault = "gone"
	_, err := e.Syncer.IncrementalWithHistoryRecovery(t.Context(), gmailCoverageSource(t, e), nil)
	require.NoError(err)
	reopenAcquisitionStore(t, e)
	// A 404 is not an authoritative empty label set. Presence reconciliation
	// tombstones the missing message while retaining its archived metadata.
	f.labels["trash"] = []string{"TRASH"}
	assertRecoveryLabels(t, e, ids, f.labels)
	assertDeletedFromSource(t, e.Store, "trash", true)
	assert.Equal(4, f.rawGets)
	assert.Equal("2001", gmailCoverageSource(t, e).SyncCursor.String)
}

func TestGmailOrdinaryFullKeepsExistingRawShortcut(t *testing.T) {
	for _, mode := range []string{"ordinary", "filtered", "limited", "noresume"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			e, f, ids := recoveryLabelsEnv(t)
			options := DefaultOptions()
			switch mode {
			case "filtered":
				options.Query = "in:inbox"
			case "limited":
				options.Limit = 1
			case "noresume":
				options.NoResume = true
			}
			f.expire, f.fault = false, "read"
			e.Syncer = New(e.Syncer.client, e.Store, options)
			summary, err := e.Syncer.Full(t.Context(), testEmail)
			require.NoError(err)
			assert.Zero(summary.Errors)
			assertRecoveryLabels(t, e, ids, map[string][]string{
				"normal1": {"INBOX"}, "normal2": {"INBOX", "Label_old"}, "spam": {"SPAM"}, "trash": {"TRASH"},
			})
			assert.Equal(4, f.rawGets)
		})
	}
}
