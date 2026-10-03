package bland

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
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
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type fixture struct {
	calls           map[string]string
	ids             []string
	hook            string
	hooks           map[string]string
	corrected       string
	correctRequests int
	audio           string
	audioFail       bool
	detailFail      string
	detailRequests  map[string]int
	audioRequests   int
	pageDuplicate   bool
}

func TestPayloadFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"payload", ErrInvalidPayload, true},
		{"wrapped payload", fmt.Errorf("decode: %w", ErrInvalidPayload), true},
		{"joined payloads", errors.Join(ErrInvalidPayload, fmt.Errorf("decode: %w", ErrInvalidPayload)), true},
		{"authentication", ErrAuthentication, false},
		{"joined authentication", errors.Join(ErrInvalidPayload, ErrAuthentication), false},
		{"wrapped joined authentication", fmt.Errorf("fetch: %w", errors.Join(ErrInvalidPayload, ErrAuthentication)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, onlyInvalidPayload(tc.err))
		})
	}
}

func (f *fixture) serve(t *testing.T) *httptest.Server {
	t.Helper()
	assertions := assert.New(t)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal("GET", r.Method)
		assertions.Equal("test-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/calls":
			offset, _ := strconv.Atoi(r.URL.Query().Get("from"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if f.pageDuplicate {
				offset = 0
			}
			end := min(offset+limit, len(f.ids))
			if offset > len(f.ids) {
				offset = len(f.ids)
			}
			calls := []map[string]string{}
			for _, id := range f.ids[offset:end] {
				calls = append(calls, map[string]string{"call_id": id})
			}
			total := len(f.ids)
			if f.pageDuplicate {
				total++
			}
			b, _ := json.Marshal(map[string]any{"calls": calls, "count": len(calls), "total_count": total})
			_, _ = w.Write(b)
		case strings.HasSuffix(r.URL.Path, "/correct"):
			f.correctRequests++
			if f.corrected == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(f.corrected))
		case strings.HasPrefix(r.URL.Path, "/v1/calls/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/calls/")
			f.detailRequests[id]++
			if id == f.detailFail {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			raw, ok := f.calls[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(raw))
		case strings.HasPrefix(r.URL.Path, "/v1/postcall/webhooks/"):
			hook := f.hook
			if f.hooks != nil {
				hook = f.hooks[strings.TrimPrefix(r.URL.Path, "/v1/postcall/webhooks/")]
			}
			if hook == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(hook))
		case strings.HasPrefix(r.URL.Path, "/v1/recordings/"):
			f.audioRequests++
			if f.audioFail {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte(f.audio))
		default:
			assertions.Fail("unexpected route", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
}
func setupImport(t *testing.T, f *fixture) (*store.Store, *Importer, ImportOptions) {
	t.Helper()
	f.detailRequests = map[string]int{}
	srv := f.serve(t)
	t.Cleanup(srv.Close)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(t, err)
	imp := NewImporter(st, NewClient(srv.URL+"/v1", "test-key"))
	imp.now = func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }
	return st, imp, ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", AttachmentsDir: t.TempDir()}
}
func loadEvidence(t *testing.T, st *store.Store, id string) *Evidence {
	t.Helper()
	requirements := require.New(t)
	src, e := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	requirements.NoError(e)
	ids, e := st.MessageExistsBatch(src.ID, []string{id})
	requirements.NoError(e)
	requirements.NotZero(ids[id])
	raw, e := st.GetMessageRaw(ids[id])
	requirements.NoError(e)
	var ev Evidence
	requirements.NoError(json.Unmarshal(raw, &ev))
	return &ev
}
func TestImporterLateTranscriptAndUnchangedRecordingRetry(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	raw := `{"call_id":"call-1","completed":true,"record":true,"created_at":"2026-10-01T11:00:00Z","corrected_duration":"60","from":"+12025550100","to":"+12025550101"}`
	f := &fixture{calls: map[string]string{"call-1": raw}, ids: []string{"call-1"}, audio: `{"error":"CALL_RECORDING_NOT_FOUND"}`}
	st, imp, o := setupImport(t, f)
	sum, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(int64(1), sum.MeetingsAdded)
	ev := loadEvidence(t, st, "call-1")
	assertions.Equal(meetingcontent.StateUnavailable, ev.Content.Transcript.State)
	assertions.Equal(attachmentpolicy.StatePending, ev.Recording.State)
	f.hook = `{"data":{"call_id":"call-1","payload":{"corrected_transcript":[{"text":"retained searchable transcript","speaker_label":"user","start":0,"end":1}]}}}`
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal("user: retained searchable transcript", loadEvidence(t, st, "call-1").Content.Transcript.Text)
	f.hook = ""
	f.audio = "ID3synthetic-audio-data"
	// The next due ID must be retried even when the discovery limit is one and
	// the provider list window no longer returns the call at all.
	f.ids = nil
	imp.now = func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) }
	o.Limit = 1
	sum, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(int64(1), sum.MaintenanceRetries)
	ev = loadEvidence(t, st, "call-1")
	assertions.Equal(attachmentpolicy.StateStored, ev.Recording.State)
	assertions.Contains(ev.Content.Transcript.Text, "searchable")
	physical, err := attachmentstore.New(store.NewPackCatalog(st), o.AttachmentsDir)
	requirements.NoError(err)
	defer func() { _ = physical.Close() }()
	data, _, err := physical.ReadBounded(ev.Recording.Hash, 1000)
	requirements.NoError(err)
	assertions.Equal([]byte(f.audio), data)
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	var messages, attachments, conversations int
	requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&messages))
	requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM attachments").Scan(&attachments))
	requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM conversations").Scan(&conversations))
	assertions.Equal(1, messages)
	assertions.Equal(1, attachments)
	assertions.Equal(1, conversations)
	// A later transcript update must preserve both the occurrence and the
	// message-level attachment flags when the stored audio is not refetched.
	f.ids = []string{"call-1"}
	f.hook = `{"data":{"call_id":"call-1","payload":{"corrected_transcript":[{"text":"updated searchable transcript","speaker_label":"user","start":0,"end":1}]}}}`
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	var attachmentCount int
	var hasAttachments bool
	requirements.NoError(st.DB().QueryRow("SELECT attachment_count,has_attachments FROM messages").Scan(&attachmentCount, &hasAttachments))
	assertions.Equal(1, attachmentCount)
	assertions.True(hasAttachments)

	assertions.Equal(3, f.audioRequests)
	_, found, err := st.SearchMessages("searchable", 0, 10)
	requirements.NoError(err)
	assertions.EqualValues(1, found)
	src, e := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	requirements.NoError(e)
	ids, e := st.MessageExistsBatch(src.ID, []string{"call-1"})
	requirements.NoError(e)
	me, e := st.GetMessageIsFromMe(ids["call-1"])
	requirements.NoError(e)
	assertions.False(me)
}
func TestImporterFailureKeepsSearchableWritesAndRetries(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := &fixture{calls: map[string]string{"call-1": `{"call_id":"call-1","completed":true,"record":true,"transcripts":[{"text":"durable evidence","user":"user"}]}`}, ids: []string{"call-1"}, audioFail: true}
	st, imp, o := setupImport(t, f)
	sum, err := imp.Import(t.Context(), o)
	requirements.Error(err)
	assertions.Equal(int64(1), sum.MeetingsAdded)
	ev := loadEvidence(t, st, "call-1")
	assertions.Equal(attachmentpolicy.StateFailed, ev.Recording.State)
	src, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	requirements.NoError(err)
	assertions.False(src.SyncCursor.Valid)
	_, found, err := st.SearchMessages("durable", 0, 10)
	requirements.NoError(err)
	assertions.EqualValues(1, found)
	f.audioFail = false
	f.audio = "ID3retry-audio-data"
	f.ids = nil
	imp.now = func() time.Time { return time.Date(2026, 10, 1, 19, 0, 0, 0, time.UTC) }
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(attachmentpolicy.StateStored, loadEvidence(t, st, "call-1").Recording.State)
}
func TestLimitedRunsProgressAndEqualTimestamps(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := &fixture{calls: map[string]string{}, ids: []string{"call-1", "call-2", "call-3"}}
	for _, id := range f.ids {
		f.calls[id] = fmt.Sprintf(`{"call_id":%q,"completed":true,"created_at":"2026-10-01T11:00:00Z","transcripts":[{"text":"meeting text","user":"user"}]}`, id)
	}
	st, imp, o := setupImport(t, f)
	o.Limit = 1
	for i := range 3 {
		sum, err := imp.Import(t.Context(), o)
		requirements.NoError(err)
		assertions.Equal(int64(1), sum.MeetingsAdded)
		assertions.Equal(i < 2, sum.PartialCoverage)
	}
	src, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	requirements.NoError(err)
	var state syncState
	requirements.NoError(json.Unmarshal([]byte(src.SyncCursor.String), &state))
	assertions.Equal("2026-10-01", state.Watermark)
	var n int
	requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&n))
	assertions.Equal(3, n)
}

func TestFullAfterFiltersArchivedCallsByCreationDate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := &fixture{calls: map[string]string{
		"call-old": `{"call_id":"call-old","completed":true,"created_at":"2020-01-01T11:00:00Z","corrected_duration":"60","transcripts":[{"text":"old meeting","user":"user"}]}`,
		"call-new": `{"call_id":"call-new","completed":true,"created_at":"2026-10-01T11:00:00Z","corrected_duration":"60","transcripts":[{"text":"new meeting","user":"user"}]}`,
	}, ids: []string{"call-old", "call-new"}}
	_, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(1, f.detailRequests["call-old"])
	assertions.Equal(1, f.detailRequests["call-new"])

	f.ids = nil
	o.Full = true
	o.CreatedAfter = time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	sum, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(int64(1), sum.MeetingsProcessed)
	assertions.Equal(1, f.detailRequests["call-old"], "archived history before --after must not be replayed")
	assertions.Equal(2, f.detailRequests["call-new"], "eligible pending work may still be reconciled")
}

func TestFutureRetryDoesNotConsumeLimitedDiscovery(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := &fixture{calls: map[string]string{
		"call-1": `{"call_id":"call-1","completed":true,"created_at":"2026-10-01T11:00:00Z","transcripts":[{"text":"first meeting","user":"user"}]}`,
		"call-2": `{"call_id":"call-2","completed":true,"created_at":"2026-10-01T11:00:00Z","transcripts":[{"text":"second meeting","user":"user"}]}`,
	}, ids: []string{"call-1", "call-2"}, detailFail: "call-1"}
	_, imp, o := setupImport(t, f)
	o.Limit = 1
	_, err := imp.Import(t.Context(), o)
	requirements.Error(err)
	assertions.Equal(1, f.detailRequests["call-1"])
	assertions.Zero(f.detailRequests["call-2"])

	imp.now = func() time.Time { return time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC) }
	sum, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.EqualValues(1, sum.MeetingsAdded)
	assertions.Equal(1, f.detailRequests["call-1"], "a retry scheduled for later must not run early")
	assertions.Equal(1, f.detailRequests["call-2"], "the future retry must leave the discovery limit for a later call")

	f.detailFail = ""
	f.ids = nil
	imp.now = func() time.Time { return time.Date(2026, 10, 1, 19, 0, 0, 0, time.UTC) }
	sum, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.EqualValues(2, sum.MaintenanceRetries)
	assertions.Equal(2, f.detailRequests["call-1"], "the failed call is retried when its scheduled time arrives")
}

func TestFutureRetryIsDeferredWithoutLimit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := &fixture{calls: map[string]string{
		"call-1": `{"call_id":"call-1","completed":true,"created_at":"2026-10-01T11:00:00Z","transcripts":[{"text":"first meeting","user":"user"}]}`,
		"call-2": `{"call_id":"call-2","completed":true,"created_at":"2026-10-01T11:00:00Z","transcripts":[{"text":"second meeting","user":"user"}]}`,
	}, ids: []string{"call-1"}, detailFail: "call-1"}
	_, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.Error(err)
	assertions.Equal(1, f.detailRequests["call-1"])

	f.detailFail = ""
	f.ids = []string{"call-1", "call-2"}
	imp.now = func() time.Time { return time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC) }
	sum, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(int64(1), sum.MeetingsAdded)
	assertions.Equal(1, f.detailRequests["call-1"], "an unlimited ordinary run must defer the future retry")
	assertions.Equal(1, f.detailRequests["call-2"], "an unlimited ordinary run must still discover new calls")

	o.Full = true
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(2, f.detailRequests["call-1"], "an explicit full run must retry even before the scheduled time")
}

func TestMissingPostCallRetainsArchivedStartAndParticipant(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	retainedPostCall := `{"data":{"payload":{"call_id":"call-1","started_at":"2026-10-01T11:00:00Z","from":"+12025550100","to":"+12025550101","summary":"retained meeting summary"}}}`
	f := &fixture{
		calls: map[string]string{"call-1": `{"call_id":"call-1","completed":true}`},
		ids:   []string{"call-1"},
		hooks: map[string]string{"call-1": retainedPostCall},
	}
	st, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.NoError(err)

	f.calls["call-1"] = `{"call_id":"call-1","completed":true}`
	f.hooks["call-1"] = ""
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)

	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	requirements.NoError(err)
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"call-1"})
	requirements.NoError(err)
	message, err := st.GetMessage(messageIDs["call-1"])
	requirements.NoError(err)
	assertions.True(message.SentAt.Equal(time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)))
	assertions.Contains(message.To, "+12025550101")
	evidence := loadEvidence(t, st, "call-1")
	assertions.Equal("retained meeting summary", evidence.Content.Summary.Text)
	assertions.JSONEq(retainedPostCall, string(evidence.PostCall))
}

func TestConsecutiveSparseRefreshesRetainEffectiveCallMetadata(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	const createdAt = "2026-10-01T11:00:00Z"
	f := &fixture{
		calls: map[string]string{"call-1": `{"call_id":"call-1","completed":true,"created_at":"2026-10-01T11:00:00Z","started_at":"2026-10-01T11:00:00Z","from":"+12025550100","to":"+12025550101","summary":"retained meeting summary"}`},
		ids:   []string{"call-1"},
	}
	st, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.NoError(err)

	o.Full = true
	f.calls["call-1"] = `{"call_id":"call-1","completed":true}`
	for range 2 {
		_, err = imp.Import(t.Context(), o)
		requirements.NoError(err)
	}

	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	requirements.NoError(err)
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"call-1"})
	requirements.NoError(err)
	evidence := loadEvidence(t, st, "call-1")
	assertions.JSONEq(`{"call_id":"call-1","completed":true}`, string(evidence.Call))
	archivedRaw, err := st.GetMessageRaw(messageIDs["call-1"])
	requirements.NoError(err)
	var evidenceFields map[string]jsontext.Value
	requirements.NoError(json.Unmarshal(archivedRaw, &evidenceFields))
	requirements.NotEmpty(evidenceFields["effective_call"])
	var effective Call
	requirements.NoError(json.Unmarshal(evidenceFields["effective_call"], &effective))
	assertions.Equal(createdAt, effective.CreatedAt)
	assertions.Equal(createdAt, effective.StartedAt)
	assertions.Equal("+12025550100", effective.From)
	assertions.Equal("+12025550101", effective.To)
	assertions.Equal("retained meeting summary", effective.Summary)

	message, err := st.GetMessage(messageIDs["call-1"])
	requirements.NoError(err)
	assertions.True(message.SentAt.Equal(time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)))
	assertions.Contains(message.To, "+12025550101")

	included, err := archivedCallCreatedAfter(t.Context(), st, messageIDs["call-1"], time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	requirements.NoError(err)
	assertions.True(included, "full replay with --after must use accumulated created_at metadata")
}

func TestNonemptyPostCallMissingMetadataRetainsArchivedFields(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	initialHook := `{"data":{"payload":{"call_id":"call-1","created_at":"2026-10-01T11:00:00Z","started_at":"2026-10-01T11:00:00Z","from":"+12025550100","to":"+12025550101","summary":"original summary"}}}`
	refreshedHook := `{"data":{"payload":{"call_id":"call-1","summary":"updated summary"}}}`
	f := &fixture{
		calls: map[string]string{"call-1": `{"call_id":"call-1","completed":true}`},
		ids:   []string{"call-1"},
		hooks: map[string]string{"call-1": initialHook},
	}
	st, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.NoError(err)

	o.Full = true
	f.hooks["call-1"] = refreshedHook
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)

	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	requirements.NoError(err)
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"call-1"})
	requirements.NoError(err)
	message, err := st.GetMessage(messageIDs["call-1"])
	requirements.NoError(err)
	assertions.True(message.SentAt.Equal(time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)))
	assertions.Contains(message.To, "+12025550101")
	evidence := loadEvidence(t, st, "call-1")
	assertions.Equal("updated summary", evidence.Content.Summary.Text)
	assertions.JSONEq(refreshedHook, string(evidence.PostCall))
}

func TestFullLimitedRunDoesNotChargeReplayedExpiredCallToDiscovery(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := &fixture{
		calls: map[string]string{
			"call-old": `{"call_id":"call-old","completed":true,"started_at":"2026-01-01T00:00:00Z","corrected_duration":"60","transcripts":[{"user":"user","text":"archived call"}]}`,
			"call-new": `{"call_id":"call-new","completed":true,"started_at":"2026-10-01T11:00:00Z","corrected_duration":"60","transcripts":[{"user":"user","text":"new call"}]}`,
		},
		ids: []string{"call-old"},
	}
	_, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(1, f.detailRequests["call-old"])

	f.ids = []string{"call-old", "call-new"}
	o.Full = true
	o.Limit = 1
	sum, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.EqualValues(1, sum.MeetingsAdded)
	assertions.Equal(2, f.detailRequests["call-old"], "full archive replay should refresh the old call once")
	assertions.Equal(1, f.detailRequests["call-new"], "the discovery budget should remain available for the new call")
	assertions.False(sum.PartialCoverage)
}

func TestProxyRetryWaitsUntilScheduledTime(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := &fixture{
		calls: map[string]string{
			"parent": `{"call_id":"parent","completed":true,"started_at":"2026-10-01T11:00:00Z","summary":"parent summary"}`,
			"child":  `{"call_id":"child","completed":true,"started_at":"2026-10-01T11:10:00Z","transcripts":[{"user":"user","text":"proxy call"}]}`,
		},
		ids:        []string{"parent"},
		hooks:      map[string]string{"parent": `{"data":{"payload":{"call_id":"parent","warm_transfer_call":{"proxy_agent_calls":[{"call_id":"child"}]}}}}`},
		detailFail: "child",
	}
	st, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.Error(err)
	assertions.Equal(1, f.detailRequests["child"])

	f.ids = nil
	imp.now = func() time.Time { return time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC) }
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(1, f.detailRequests["child"], "an existing proxy retry must not run before Next")

	f.detailFail = ""
	imp.now = func() time.Time { return time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC) }
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(2, f.detailRequests["child"], "a proxy retry must run once it is due")
	assertions.Equal("parent", loadEvidence(t, st, "child").ParentCallID)
}

func TestFullLimitedRunKeepsNewDiscoveryBudgetAfterMaintenance(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	ids := []string{"call-1", "call-2", "call-3", "call-4", "call-5"}
	calls := make(map[string]string, len(ids)+1)
	for _, id := range append(append([]string(nil), ids...), "call-6") {
		calls[id] = fmt.Sprintf(`{"call_id":%q,"completed":true,"created_at":"2026-10-01T11:00:00Z","transcripts":[{"text":%q,"user":"user"}]}`, id, id+" meeting")
	}
	f := &fixture{calls: calls, ids: append([]string(nil), ids...)}
	_, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(1, f.detailRequests["call-1"])

	f.ids = append(f.ids, "call-6")
	o.Full = true
	o.Limit = 1
	sum, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.EqualValues(1, sum.MeetingsAdded, "the limit is reserved for a newly discovered call")
	assertions.Equal(1, f.detailRequests["call-6"])
	assertions.False(sum.PartialCoverage, "processed maintenance calls must not exhaust the discovery budget")
}

func TestRecordingOccurrenceWriteFailureRecoversFromStoredDescriptor(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite-only trigger to simulate an attachment occurrence write failure")
	assertions := assert.New(t)
	requirements := require.New(t)
	f := &fixture{
		calls: map[string]string{"call-1": `{"call_id":"call-1","completed":true,"record":true,"transcripts":[{"text":"durable evidence","user":"user"}]}`},
		ids:   []string{"call-1"},
		audio: "ID3synthetic-audio-data",
	}
	st, imp, o := setupImport(t, f)
	_, err := st.DB().Exec(`CREATE TRIGGER fail_bland_recording_occurrence
		BEFORE INSERT ON attachments
		WHEN NEW.source_attachment_id = 'bland:recording:call-1'
		BEGIN SELECT RAISE(ABORT, 'synthetic attachment occurrence failure'); END`)
	requirements.NoError(err)
	_, err = imp.Import(t.Context(), o)
	requirements.Error(err)
	stored := loadEvidence(t, st, "call-1").Recording
	requirements.Equal(attachmentpolicy.StateStored, stored.State)
	requirements.NotEmpty(stored.Hash)
	requirements.NotEmpty(stored.Path)
	var occurrences int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE source_attachment_id = 'bland:recording:call-1'`).Scan(&occurrences))
	assertions.Zero(occurrences)

	_, err = st.DB().Exec(`DROP TRIGGER fail_bland_recording_occurrence`)
	requirements.NoError(err)
	o.Full = true
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE source_attachment_id = 'bland:recording:call-1'`).Scan(&occurrences))
	assertions.Equal(1, occurrences)
	assertions.Equal(1, f.audioRequests, "a stored descriptor should repair the row without downloading audio again")
}

func TestRecordingPolicyAndByteCap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy attachmentpolicy.Policy
		want   attachmentpolicy.SkipReason
	}{
		{"none", attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone}, attachmentpolicy.SkipPolicyScope},
		{"size", attachmentpolicy.Policy{MaxBytes: 8}, attachmentpolicy.SkipSizeCap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			f := &fixture{calls: map[string]string{"call-1": `{"call_id":"call-1","completed":true,"record":true}`}, ids: []string{"call-1"}, audio: "ID3too-long-audio"}
			st, imp, o := setupImport(t, f)
			o.MediaPolicy = tc.policy
			_, err := imp.Import(t.Context(), o)
			requirements.NoError(err)
			ev := loadEvidence(t, st, "call-1")
			assertions.Equal(attachmentpolicy.StateSkipped, ev.Recording.State)
			assertions.Equal(tc.want, ev.Recording.SkipReason)
			entries, err := os.ReadDir(o.AttachmentsDir)
			requirements.NoError(err)
			assertions.Empty(entries)
		})
	}
}

func TestSizeCapSkipIsReconsideredOnlyOnFullSync(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := &fixture{calls: map[string]string{"call-1": `{"call_id":"call-1","completed":true,"record":true}`}, ids: []string{"call-1"}, audio: "ID3too-long-audio"}
	st, imp, o := setupImport(t, f)
	o.MediaPolicy = attachmentpolicy.Policy{MaxBytes: 8}
	_, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	requirements.Equal(1, f.audioRequests)
	ev := loadEvidence(t, st, "call-1")
	requirements.Equal(attachmentpolicy.StateSkipped, ev.Recording.State)
	requirements.Equal(attachmentpolicy.SkipSizeCap, ev.Recording.SkipReason)

	firstAttempt := imp.now()
	imp.now = func() time.Time { return firstAttempt.Add(6 * time.Hour) }
	f.ids = nil
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(1, f.audioRequests, "an ordinary retry must not redownload a size-capped recording")

	o.Full = true
	o.MediaPolicy = attachmentpolicy.Policy{MaxBytes: 100}
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(2, f.audioRequests, "a full sync must reconsider the skipped recording after a policy change")
	assertions.Equal(attachmentpolicy.StateStored, loadEvidence(t, st, "call-1").Recording.State)
}

func TestProxyCallSeparateAndDeletedSourceNotRecreated(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := &fixture{ids: []string{"parent"}, calls: map[string]string{
		"parent": `{"call_id":"parent","completed":true,"record":true,"warm_transfer_call":{"proxy_agent_calls":[{"call_id":"child"}]}}`,
		"child":  `{"call_id":"child","completed":true,"transcripts":[{"text":"separate leg","user":"robot"}]}`,
	}, audio: "ID3parent-recording"}
	st, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal("parent", loadEvidence(t, st, "child").ParentCallID)
	ev := loadEvidence(t, st, "parent")
	_, err = os.Stat(filepath.Join(o.AttachmentsDir, ev.Recording.Path))
	requirements.NoError(err)
	o.Identifier = "removed"
	_, err = imp.Import(t.Context(), o)
	requirements.ErrorIs(err, store.ErrSourceNotFound)
	sources, err := st.ListSources(SourceType)
	requirements.NoError(err)
	assertions.Len(sources, 1)
}

func TestOptionalCorrectedTranscriptRetrieval(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := &fixture{calls: map[string]string{"call-1": `{"call_id":"call-1","completed":true,"transcripts":[{"user":"user","text":"ordinary"}]}`}, ids: []string{"call-1"}, corrected: `{"status":"success","corrected":[{"speaker_label":"user","speaker":1,"start":0,"end":2,"text":"direct correction"}],"aligned":[{"text":"legacy"}]}`}
	st, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Zero(f.correctRequests)
	o.FetchCorrectedTranscript = true
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal("user: direct correction", loadEvidence(t, st, "call-1").Content.Transcript.Text)
	assertions.NotEmpty(loadEvidence(t, st, "call-1").DirectCorrection)
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	var n int
	requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&n))
	assertions.Equal(1, n)
	f.corrected = ""
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal("user: direct correction", loadEvidence(t, st, "call-1").Content.Transcript.Text)
	f.corrected = `{"status":"error","message":"provider-private-token"}`
	_, err = imp.Import(t.Context(), o)
	requirements.Error(err)
	assertions.NotContains(err.Error(), "provider-private-token")
	assertions.Equal("user: direct correction", loadEvidence(t, st, "call-1").Content.Transcript.Text)
}

func TestImporterUsesCurrentRetainedCorrectionWhenDirectCorrectionIsUnavailable(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	const directCorrection = `{"status":"success","corrected":[{"text":"direct orchid correction","speaker_label":"user","start":0,"end":1}],"aligned":[]}`
	f := &fixture{
		calls: map[string]string{
			"call-1": `{"call_id":"call-1","completed":true,"started_at":"2026-10-01T11:00:00Z","corrected_duration":"60","transcripts":[{"text":"ordinary speech","user":"user"}]}`,
		},
		ids:       []string{"call-1"},
		hook:      `{"data":{"call_id":"call-1","payload":{"corrected_transcript":[{"text":"initial retained transcript","speaker_label":"user","start":0,"end":1}]}}}`,
		corrected: directCorrection,
	}
	st, imp, options := setupImport(t, f)
	options.FetchCorrectedTranscript = true
	_, err := imp.Import(t.Context(), options)
	requirements.NoError(err)
	first := loadEvidence(t, st, "call-1")
	assertions.Equal("user: direct orchid correction", first.Content.Transcript.Text)
	assertions.JSONEq(directCorrection, string(first.DirectCorrection))

	firstSync := imp.now()
	imp.now = func() time.Time { return firstSync.Add(6 * time.Hour) }
	f.hook = `{"data":{"call_id":"call-1","payload":{"corrected_transcript":[{"text":"freshly retained transcript","speaker_label":"user","start":0,"end":1}]}}}`
	f.corrected = ""
	_, err = imp.Import(t.Context(), options)
	requirements.NoError(err)
	second := loadEvidence(t, st, "call-1")
	assertions.Equal("user: freshly retained transcript", second.Content.Transcript.Text)
	assertions.JSONEq(string(first.DirectCorrection), string(second.DirectCorrection))
	assertions.Equal(2, f.correctRequests, "the second sync must receive the direct endpoint's 404")

	_, found, err := st.SearchMessages("freshly", 0, 10)
	requirements.NoError(err)
	assertions.EqualValues(1, found)
	_, found, err = st.SearchMessages("orchid", 0, 10)
	requirements.NoError(err)
	assertions.EqualValues(0, found)
}

func TestRetainedPayloadRecordingWAVAndProxy(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := &fixture{calls: map[string]string{"parent": `{"call_id":"parent","queue_status":"complete_error"}`, "proxy": `{"call_id":"proxy","completed":true,"transcripts":[{"text":"proxy conversation","user":"user"}]}`}, ids: []string{"parent"}, audio: "RIFF\x10\x00\x00\x00WAVEsynthetic-audio", hooks: map[string]string{"parent": `{"data":{"call_id":"parent","payload":{"call_id":"parent","record":true,"recording_url":"https://unused.example/audio","summary":"retained summary","corrected_duration":"25","transcripts":[{"text":"retained speech","user":"user"}],"warm_transfer_call":{"proxy_agent_calls":[{"call_id":"proxy"}]}}}}`}}
	st, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	ev := loadEvidence(t, st, "parent")
	assertions.Equal("retained summary", ev.Content.Summary.Text)
	assertions.Equal("user: retained speech", ev.Content.Transcript.Text)
	assertions.Equal("audio/wav", ev.Recording.MIME)
	assertions.Equal("parent", loadEvidence(t, st, "proxy").ParentCallID)
	var filename, mime string
	var count int
	var flag bool
	requirements.NoError(st.DB().QueryRow("SELECT filename,mime_type FROM attachments").Scan(&filename, &mime))
	assertions.Equal("call-recording.wav", filename)
	assertions.Equal("audio/wav", mime)
	requirements.NoError(st.DB().QueryRow("SELECT attachment_count,has_attachments FROM messages WHERE source_message_id = 'parent'").Scan(&count, &flag))
	assertions.Equal(1, count)
	assertions.True(flag)
}

func TestImporterArchivesSummaryWithoutTranscriptOrRecording(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := &fixture{
		calls: map[string]string{"call-1": `{"call_id":"call-1","completed":true}`},
		ids:   []string{"call-1"},
		hook:  `{"data":{"payload":{"call_id":"call-1","summary":"retained call summary"}}}`,
	}
	st, imp, o := setupImport(t, f)
	summary, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	requirements.Equal(int64(1), summary.MeetingsAdded)

	ev := loadEvidence(t, st, "call-1")
	assertions.Equal(meetingcontent.StateAvailable, ev.Content.Summary.State)
	assertions.Equal("retained call summary", ev.Content.Summary.Text)
	assertions.Equal(meetingcontent.StateUnavailable, ev.Content.Transcript.State)
	_, found, err := st.SearchMessages("retained call summary", 0, 10)
	requirements.NoError(err)
	assertions.EqualValues(1, found)
}

func TestExpiredRecordingCoverageAndFullRecovery(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := &fixture{calls: map[string]string{"call-1": `{"call_id":"call-1","completed":true,"record":true,"started_at":"2026-10-01T11:00:00Z","corrected_duration":"60"}`}, ids: []string{"call-1"}, audio: `{"error":"CALL_RECORDING_NOT_FOUND"}`}
	st, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	f.ids = nil
	imp.now = func() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC) }
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(attachmentpolicy.StateUnavailable, loadEvidence(t, st, "call-1").Recording.State)
	calls := f.audioRequests
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(calls, f.audioRequests)
	f.audio = "ID3late-recovered-audio"
	o.Full = true
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(attachmentpolicy.StateStored, loadEvidence(t, st, "call-1").Recording.State)
}

func TestRepeatedPageDoesNotAdvanceCursor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := &fixture{calls: map[string]string{"call-1": `{"call_id":"call-1","completed":true,"transcripts":[{"text":"searchable preserved speech","user":"user"}]}`}, ids: []string{"call-1"}, pageDuplicate: true}
	st, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.Error(err)
	assertions.Contains(err.Error(), "repeated pagination")
	src, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	requirements.NoError(err)
	assertions.False(src.SyncCursor.Valid)
	assertions.Contains(loadEvidence(t, st, "call-1").Content.Transcript.Text, "preserved speech")
	f.pageDuplicate = false
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	src, err = st.GetSourceByTypeAndIdentifier(SourceType, "work")
	requirements.NoError(err)
	assertions.True(src.SyncCursor.Valid)
	var count int
	requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&count))
	assertions.Equal(1, count)
}

func TestTerminalLedgerPrunedAndFullUsesArchive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := &fixture{calls: map[string]string{}, ids: []string{"call-1", "call-2", "call-3"}}
	for _, id := range f.ids {
		f.calls[id] = fmt.Sprintf(`{"call_id":%q,"completed":true,"started_at":"2026-01-01T00:00:00Z","corrected_duration":"60","transcripts":[{"user":"user","text":"historical speech"}]}`, id)
	}
	st, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	src, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	requirements.NoError(err)
	var cursor syncState
	requirements.NoError(json.Unmarshal([]byte(src.SyncCursor.String), &cursor))
	assertions.Empty(cursor.Pending, "expired reconciliation records must not grow forever")
	f.ids = nil
	o.Full = true
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	for id := range f.calls {
		assertions.Equal(2, f.detailRequests[id], "full rechecks archived IDs omitted by discovery")
	}
}

func TestCheckpointSnapshotVolumeBounded(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := &fixture{calls: map[string]string{}}
	for index := range 201 {
		id := fmt.Sprintf("call-%03d", index)
		f.ids = append(f.ids, id)
		f.calls[id] = fmt.Sprintf(`{"call_id":%q,"status":"busy"}`, id)
	}
	srv := f.serve(t)
	defer srv.Close()
	f.detailRequests = map[string]int{}
	st := testutil.NewSQLiteTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "work")
	requirements.NoError(err)
	// Count real database publication rather than wall-clock throughput.
	_, err = st.DB().Exec(`CREATE TABLE test_cursor_writes(size INTEGER NOT NULL);
 CREATE TRIGGER test_count_cursor_writes AFTER UPDATE OF cursor_before ON sync_runs
 WHEN length(NEW.cursor_before)>0 BEGIN INSERT INTO test_cursor_writes VALUES(length(NEW.cursor_before)); END;`)
	requirements.NoError(err)
	imp := NewImporter(st, NewClient(srv.URL+"/v1", "test-key"))
	imp.now = func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }
	o := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", AttachmentsDir: t.TempDir()}
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	var writes, totalBytes int
	requirements.NoError(st.DB().QueryRow("SELECT COUNT(*),COALESCE(SUM(size),0) FROM test_cursor_writes").Scan(&writes, &totalBytes))
	assertions.LessOrEqual(writes, 18, "16 intermediate plus initial/final snapshots")
	assertions.Len(f.detailRequests, 201)
	src, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	requirements.NoError(err)
	var state syncState
	requirements.NoError(json.Unmarshal([]byte(src.SyncCursor.String), &state))
	assertions.Len(state.Pending, 201)
	assertions.LessOrEqual(totalBytes, 20*len(src.SyncCursor.String), "snapshot bytes scale with final cursor size")
}

func TestMalformedRenditionKeepsValidArtifactsAndAgesOut(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := &fixture{calls: map[string]string{"call-1": `{"call_id":"call-1","completed":true,"started_at":"2026-10-01T11:00:00Z","corrected_duration":"60","record":true,"transcripts":[{"user":"user","text":"valid ordinary speech"}]}`}, ids: []string{"call-1"}, hook: `{"data":{"payload":{"corrected_transcript":[{"speaker_label":"user","text":"malformed correction","start":-1,"end":2}]}}}`, audio: "ID3valid-retained-audio"}
	st, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.Error(err)
	ev := loadEvidence(t, st, "call-1")
	assertions.Equal("user: valid ordinary speech", ev.Content.Transcript.Text)
	assertions.Equal(attachmentpolicy.StateStored, ev.Recording.State)
	src, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	requirements.NoError(err)
	assertions.False(src.SyncCursor.Valid)
	var failed syncState
	run, err := st.GetLatestCheckpointedSyncByType(src.ID, SourceType)
	requirements.NoError(err)
	requirements.NoError(json.Unmarshal([]byte(run.CursorBefore.String), &failed))
	assertions.Equal(imp.now().Add(6*time.Hour), failed.Pending["call-1"].Next)
	imp.now = func() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC) }
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	src, err = st.GetSourceByTypeAndIdentifier(SourceType, "work")
	requirements.NoError(err)
	assertions.True(src.SyncCursor.Valid)
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	requirements.NoError(json.Unmarshal([]byte(mustSourceCursor(t, st)), &failed))
	assertions.Equal("2026-10-09", failed.Watermark)
	assertions.Equal("user: valid ordinary speech", loadEvidence(t, st, "call-1").Content.Transcript.Text)
}

func TestExpiredRecordingFailureRemainsRetryable(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := &fixture{
		calls: map[string]string{"call-1": `{"call_id":"call-1","completed":true,"started_at":"2026-10-01T11:00:00Z","corrected_duration":"60","record":true,"transcripts":[{"user":"user","text":"retained transcript"}]}`},
		ids:   []string{"call-1"},
		audio: `{"error":"CALL_RECORDING_NOT_FOUND"}`,
	}
	st, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.NoError(err)

	f.audioFail = true
	f.ids = nil
	imp.now = func() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC) }
	_, err = imp.Import(t.Context(), o)
	requirements.Error(err)
	src, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	requirements.NoError(err)
	run, err := st.GetLatestCheckpointedSyncByType(src.ID, SourceType)
	requirements.NoError(err)
	var failed syncState
	requirements.NoError(json.Unmarshal([]byte(run.CursorBefore.String), &failed))
	requirements.Contains(failed.Pending, "call-1")
	assertions.False(failed.Pending["call-1"].Terminal)
	assertions.Equal(imp.now().Add(6*time.Hour), failed.Pending["call-1"].Next)

	f.audioFail = false
	f.audio = "ID3synthetic-recovered-audio"
	imp.now = func() time.Time { return time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC) }
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(attachmentpolicy.StateStored, loadEvidence(t, st, "call-1").Recording.State)
}

func mustSourceCursor(t *testing.T, st *store.Store) string {
	t.Helper()
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	require.NoError(t, err)
	return source.SyncCursor.String
}

func TestExpiredInvalidArtifactFreeCallCountedOnce(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := &fixture{calls: map[string]string{"call-1": `{"call_id":"call-1","completed":true,"started_at":"2026-10-01T11:00:00Z","corrected_duration":"60"}`}, ids: []string{"call-1"}, hook: `{"data":{"payload":{"corrected_transcript":[{"text":"malformed correction","start":-1,"end":2}]}}}`}
	_, imp, o := setupImport(t, f)
	_, err := imp.Import(t.Context(), o)
	requirements.Error(err)
	imp.now = func() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC) }
	summary, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(int64(1), summary.Skipped)
	assertions.Equal(int64(1), summary.Errors)
}
