package bland

import (
	"cmp"
	"encoding/json/jsontext"
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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const notReadyAudio = `{"data":null,"errors":[{"error":"CALL_RECORDING_NOT_FOUND","message":"Call recording not found"}]}`

type fixture struct {
	calls map[string]string
	// details, when set, answers a call's details read instead of calls.
	details map[string]string
	// rows, when set, answers a call's listing row instead of one from calls.
	rows  map[string]string
	ids   []string
	hook  string
	hooks map[string]string
	audio string
	// audios, when set, answers each call's recording; a call without one
	// gets Bland's not-ready body.
	audios         map[string]string
	audioStatus    int
	hookStatus     int
	detailStatus   int
	detailRequests map[string]int
	audioRequests  map[string]int
	pageCap        int
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
			ids := f.listed(r.URL.Query().Get("start_date"))
			offset = min(offset, len(ids))
			if f.pageCap > 0 {
				limit = min(limit, f.pageCap)
			}
			calls := []jsontext.Value{}
			for _, id := range ids[offset:min(offset+limit, len(ids))] {
				row := listRow(id, f.calls[id])
				if raw, ok := f.rows[id]; ok {
					row = jsontext.Value(raw)
				}
				calls = append(calls, row)
			}
			b, _ := json.Marshal(map[string]any{"calls": calls, "count": len(calls), "total_count": len(ids)})
			_, _ = w.Write(b)
		case strings.HasPrefix(r.URL.Path, "/v1/calls/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/calls/")
			f.detailRequests[id]++
			if f.detailStatus != 0 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(f.detailStatus)
				return
			}
			raw, ok := f.details[id]
			if !ok {
				raw, ok = f.calls[id]
			}
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(raw))
		case strings.HasPrefix(r.URL.Path, "/v1/postcall/webhooks/"):
			if f.hookStatus != 0 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(f.hookStatus)
				return
			}
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
			id := strings.TrimPrefix(r.URL.Path, "/v1/recordings/")
			f.audioRequests[id]++
			if f.audioStatus != 0 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(f.audioStatus)
				return
			}
			audio := f.audio
			if f.audios != nil {
				var ok bool
				if audio, ok = f.audios[id]; !ok {
					audio = notReadyAudio
				}
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte(audio))
		default:
			assertions.Fail("unexpected route", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
}

// listRow keeps only the fields Bland documents for a call list row.
func listRow(id, detail string) jsontext.Value {
	row := map[string]jsontext.Value{}
	_ = json.Unmarshal([]byte(detail), &row)
	for key := range row {
		switch key {
		case "call_id", "created_at", "call_length", "to", "from", "completed", "inbound", "queue_status", "endpoint_url", "max_duration", "error_message", "batch_id":
		default:
			delete(row, key)
		}
	}
	if _, ok := row["call_id"]; !ok {
		row["call_id"] = jsontext.Value(strconv.Quote(id))
	}
	b, _ := json.Marshal(row, json.Deterministic(true))
	return b
}

// listed returns the calls created on or after start, a date or timestamp,
// when set; a call without created_at is always listed.
func (f *fixture) listed(start string) []string {
	bound, err := time.Parse(time.RFC3339Nano, start)
	if err != nil {
		bound, _ = time.Parse(time.DateOnly, start)
	}
	var ids []string
	for _, id := range f.ids {
		var c struct {
			CreatedAt time.Time `json:"created_at"`
		}
		if json.Unmarshal([]byte(f.calls[id]), &c) != nil || c.CreatedAt.IsZero() ||
			start == "" || !c.CreatedAt.Before(bound) {
			ids = append(ids, id)
		}
	}
	return ids
}

func setupImport(t *testing.T, f *fixture) (*store.Store, *Importer, ImportOptions) {
	t.Helper()
	f.detailRequests, f.audioRequests = map[string]int{}, map[string]int{}
	srv := f.serve(t)
	t.Cleanup(srv.Close)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(t, err)
	imp := NewImporter(st, NewClient(srv.URL+"/v1", "test-key"))
	imp.now = func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }
	return st, imp, ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", AttachmentsDir: t.TempDir()}
}

func messageID(t *testing.T, st *store.Store, id string) int64 {
	t.Helper()
	src, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	require.NoError(t, err)
	ids, err := st.MessageExistsBatch(src.ID, []string{id})
	require.NoError(t, err)
	require.NotZero(t, ids[id])
	return ids[id]
}

func loadEvidence(t *testing.T, st *store.Store, id string) *Evidence {
	t.Helper()
	raw, err := st.GetMessageRaw(messageID(t, st, id))
	require.NoError(t, err)
	var ev Evidence
	require.NoError(t, json.Unmarshal(raw, &ev))
	return &ev
}

func recordingRow(t *testing.T, st *store.Store, id string) store.AttachmentRef {
	t.Helper()
	rows, err := st.MessageProviderAttachments(messageID(t, st, id), spec.RowPrefix)
	require.NoError(t, err)
	require.Contains(t, rows, recordingKey(id))
	return rows[recordingKey(id)]
}

// Each sync relists the calls created in the window and fetches them again:
// a summary written on day 2 and a recording made on day 3 reach the meeting,
// a queued call is archived at once and updated once it ends, a call without a
// recording URL gets no recording row, an unchanged relist rewrites nothing,
// --full keeps a stored recording and the archived start time and other
// party when the details go sparse, a recording Bland never produces ages out
// unavailable, and a call past the window isn't fetched again.
func TestRelistWindowWalk(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	const at = `"created_at":"2026-10-01T11:00:00Z","started_at":"2026-10-01T11:00:00Z"`
	f := &fixture{calls: map[string]string{
		"late":    `{"call_id":"late","completed":true,"record":true,"recording_url":"https://example.invalid/late","to":"+12025550101","corrected_duration":"60",` + at + `}`,
		"missing": `{"call_id":"missing","completed":true,"record":true,"recording_url":"https://example.invalid/missing","corrected_duration":"60",` + at + `}`,
		"queued":  `{"call_id":"queued","queue_status":"queued","record":true,` + at + `}`,
	}, ids: []string{"late", "missing", "queued"}, audios: map[string]string{}}
	st, imp, o := setupImport(t, f)
	day := func(d int) { imp.now = func() time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC) } }
	sum, err := imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.EqualValues(3, sum.MeetingsAdded, "calls are archived before their artifacts")
	assertions.Equal(attachmentpolicy.StatePending, recordingRow(t, st, "late").State)
	assertions.Equal(attachmentpolicy.StatePending, recordingRow(t, st, "missing").State)
	rows, err := st.MessageProviderAttachments(messageID(t, st, "queued"), spec.RowPrefix)
	requirements.NoError(err)
	assertions.Empty(rows, "a call without a recording URL gets no recording row")
	assertions.Zero(f.audioRequests["queued"])

	// Day 2: Bland writes the summary, and the queued call ends.
	f.calls["late"] = strings.Replace(f.calls["late"], `"record":true`, `"record":true,"summary":"late summary"`, 1)
	f.calls["queued"] = `{"call_id":"queued","completed":true,"record":true,"corrected_duration":"30",` + at + `}`
	day(2)
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal("late summary", loadEvidence(t, st, "late").Content.Summary.Text)
	assertions.InDelta(30, *loadEvidence(t, st, "queued").Content.DurationSeconds, 0)

	// Day 3: the recording arrives.
	f.audios["late"] = "ID3late-audio"
	day(3)
	sum, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.EqualValues(1, sum.MeetingsUpdated, "the stored recording updates its meeting")
	assertions.Equal(attachmentpolicy.StateStored, recordingRow(t, st, "late").State)

	// Relisting unchanged calls leaves the pending rows and caches alone.
	revision, err := st.DerivedDataRevision()
	requirements.NoError(err)
	sum, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Zero(sum.MeetingsUpdated)
	after, err := st.DerivedDataRevision()
	requirements.NoError(err)
	assertions.Equal(revision, after, "an unchanged pending recording isn't rewritten")

	// --full refreshes the transcript and keeps the stored recording, and
	// sparse details keep the archived start time and other party.
	f.calls["late"] = `{"call_id":"late","completed":true,"created_at":"2026-10-01T10:59:00Z"}`
	f.hooks = map[string]string{"late": `{"data":{"payload":{"call_id":"late","corrected_transcript":[{"text":"updated speech","speaker_label":"user","start":0,"end":1}]}}}`}
	downloads := f.audioRequests["late"]
	o.Full = true
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	o.Full = false
	assertions.Equal("user: updated speech", loadEvidence(t, st, "late").Content.Transcript.Text)
	assertions.Equal(downloads, f.audioRequests["late"], "stored audio isn't downloaded again")
	message, err := st.GetMessage(messageID(t, st, "late"))
	requirements.NoError(err)
	assertions.True(message.SentAt.Equal(time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)))
	assertions.Equal("Call to +12025550101", message.Subject)
	assertions.Contains(message.To, "+12025550101")

	// Day 10 moves the window past the calls, so day 11 lists from after them.
	day(10)
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(attachmentpolicy.StateUnavailable, recordingRow(t, st, "missing").State, "Bland never produced it")
	f.calls["late"] = strings.Replace(f.calls["late"], "late summary", "too late", 1)
	fetched := f.detailRequests["late"]
	day(11)
	_, err = imp.Import(t.Context(), o)
	requirements.NoError(err)
	assertions.Equal(fetched, f.detailRequests["late"])
	assertions.Equal("late summary", loadEvidence(t, st, "late").Content.Summary.Text)
}

// Calls on later pages are listed until total_count, and a --limit run resumes
// at the page where it stopped.
func TestPagingResumesAcrossLimitedRuns(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := &fixture{calls: map[string]string{}, pageCap: 2}
	for i := range 5 {
		id := fmt.Sprintf("call-%d", i)
		f.ids = append(f.ids, id)
		f.calls[id] = fmt.Sprintf(`{"call_id":%q,"completed":true,"status":"no-answer","created_at":"2026-09-30T11:00:00Z"}`, id)
	}
	st, imp, o := setupImport(t, f)
	o.Limit = 2
	runs := 0
	for pending := true; pending; runs++ {
		sum, err := imp.Import(t.Context(), o)
		requirements.NoError(err)
		pending = sum.DiscoveryPending
	}
	assertions.Equal(3, runs)
	for _, id := range f.ids {
		assertions.Equal(1, f.detailRequests[id], id)
	}
	var archived int
	requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&archived))
	assertions.Equal(5, archived)
}

// A recording's row records how it went: one Bland refuses fails without
// failing the run, a server error fails the run at its end and leaves the row
// waiting for the next relist, the media policy skips one without requesting
// it, and WAV audio is stored as WAV.
func TestRecordingRowOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		audio    string
		skip     bool
		runFails bool
		want     attachmentpolicy.DownloadState
		mime     string
	}{
		{name: "refused", status: http.StatusBadRequest, want: attachmentpolicy.StateFailed},
		{name: "server error", status: http.StatusBadGateway, runFails: true, want: attachmentpolicy.StatePending},
		{name: "policy skip", skip: true, audio: "ID3synthetic-audio", want: attachmentpolicy.StateSkipped},
		{name: "wav", audio: "RIFF\x10\x00\x00\x00WAVEsynthetic-audio", want: attachmentpolicy.StateStored, mime: "audio/wav"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := &fixture{calls: map[string]string{"call-1": `{"call_id":"call-1","completed":true,"record":true,"recording_url":"https://example.invalid/call-1","corrected_duration":"60","created_at":"2026-10-01T11:00:00Z","transcripts":[{"user":"user","text":"durable evidence"}]}`}, ids: []string{"call-1"}, audioStatus: tc.status, audio: tc.audio}
			st, imp, o := setupImport(t, f)
			if tc.skip {
				o.MediaPolicy = attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone}
			}
			sum, err := imp.Import(t.Context(), o)
			if tc.runFails {
				requirements.ErrorContains(err, "bland call call-1")
			} else {
				requirements.NoError(err)
				failures := 0
				if tc.want == attachmentpolicy.StateFailed {
					failures = 1
				}
				assertions.EqualValues(failures, sum.Errors)
			}
			row := recordingRow(t, st, "call-1")
			assertions.Equal(tc.want, row.State)
			assertions.Contains(loadEvidence(t, st, "call-1").Content.Transcript.Text, "durable evidence")
			if tc.skip {
				assertions.Equal(attachmentpolicy.SkipPolicyScope, row.SkipReason)
				assertions.Empty(f.audioRequests)
			}
			if tc.mime != "" {
				assertions.Equal("call-recording.wav", row.Filename)
				assertions.Equal(tc.mime, row.MimeType)
			}
		})
	}
}

// A details or postcall read Bland refuses is noted and the call archives
// without it; a transient or malformed one fails the run. Malformed details
// leave the archived call as it was, and a malformed postcall archives the
// call from its details. A refused details read archives the call from its
// listed row and still reads the postcall data, and a listed row's fields are
// read only then. A transcript a failed read may have hidden is marked refused
// or unfetched, and the next relist reads it.
func TestCallReadFailures(t *testing.T) {
	const retained = `{"data":{"payload":{"call_id":"call-1","record":true,"recording_url":"https://example.invalid/call-1","corrected_transcript":[{"text":"postcall speech","speaker_label":"user","start":0,"end":1}]}}}`
	const detail = `{"call_id":"call-1","completed":true,"created_at":"2026-10-01T11:00:00Z","record":true,"recording_url":"https://example.invalid/call-1","corrected_duration":"60","summary":"detail summary"}`
	for _, tc := range []struct {
		name, detail, details, listed string
		detailStatus, hookStatus      int
		hook                          string
		runFails, archived            bool
		transcript                    string
		recorded, priorSync           bool
		// unchanged means the archive from the prior sync stays as it was.
		unchanged bool
	}{
		{name: "postcall server error", hookStatus: http.StatusServiceUnavailable, runFails: true, archived: true},
		{name: "postcall error over empty transcript", detail: `{"call_id":"call-1","completed":true,"created_at":"2026-10-01T11:00:00Z","corrected_duration":"60","summary":"detail summary","transcripts":[]}`, hookStatus: http.StatusServiceUnavailable, runFails: true, archived: true},
		{name: "postcall refused", hookStatus: http.StatusForbidden, archived: true},
		{name: "detail refused", detailStatus: http.StatusForbidden, hook: retained, archived: true, transcript: "user: postcall speech", recorded: true},
		{name: "detail refused without postcall", detailStatus: http.StatusForbidden, archived: true},
		{name: "detail refused after an archive", detailStatus: http.StatusForbidden, archived: true, priorSync: true},
		{name: "detail rate limited", detailStatus: http.StatusTooManyRequests, runFails: true},
		{name: "malformed postcall", detail: `{"call_id":"call-1","completed":true,"created_at":"2026-10-01T11:00:00Z","record":true,"recording_url":"https://example.invalid/call-1","corrected_duration":"60","transcripts":[{"user":"user","text":"valid ordinary speech"}]}`, hook: `{"data":{"payload":{"corrected_transcript":[{"speaker_label":"user","text":"malformed correction","start":-1,"end":2}]}}}`, runFails: true, archived: true, transcript: "user: valid ordinary speech", recorded: true},
		{name: "malformed details after an archive", details: `{"call_id":"call-1","created_at":"2026-10-01T11:00:00Z","transcripts":[null]}`, priorSync: true, runFails: true, archived: true, unchanged: true},
		{name: "bad listed call_length", detail: `{"call_id":"call-1","completed":true,"created_at":"2026-10-01T11:00:00Z","transcripts":[{"user":"user","text":"listed speech"}]}`, listed: `{"call_id":"call-1","call_length":"bad"}`, archived: true, transcript: "user: listed speech"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := &fixture{calls: map[string]string{"call-1": cmp.Or(tc.detail, detail)}, ids: []string{"call-1"}, detailStatus: tc.detailStatus, hookStatus: tc.hookStatus, hook: tc.hook, audio: "ID3synthetic-audio"}
			if tc.listed != "" {
				f.rows = map[string]string{"call-1": tc.listed}
			}
			if tc.unchanged {
				f.calls["call-2"], f.ids = strings.ReplaceAll(detail, "call-1", "call-2"), append(f.ids, "call-2")
			}
			st, imp, o := setupImport(t, f)
			if tc.priorSync {
				f.detailStatus = 0
				_, err := imp.Import(t.Context(), o)
				requirements.NoError(err)
				f.detailStatus = tc.detailStatus
			}
			var prior []byte
			if tc.unchanged {
				var err error
				prior, err = st.GetMessageRaw(messageID(t, st, "call-1"))
				requirements.NoError(err)
			}
			if tc.details != "" {
				f.details = map[string]string{"call-1": tc.details}
			}
			sum, err := imp.Import(t.Context(), o)
			if tc.runFails {
				requirements.ErrorContains(err, "bland call call-1")
			} else {
				requirements.NoError(err)
				assertions.Zero(sum.Errors, "a refused read is a note")
				notes := 0
				if tc.detailStatus != 0 || tc.hookStatus != 0 {
					notes = 1
				}
				assertions.Len(sum.Diagnostics, notes)
			}
			if !tc.archived {
				exists, err := st.MessageExistsBatch(sum.SourceID, []string{"call-1"})
				requirements.NoError(err)
				assertions.Zero(exists["call-1"])
				return
			}
			if tc.unchanged {
				requirements.ErrorContains(err, "transcripts", "the error names the malformed field")
				raw, err := st.GetMessageRaw(messageID(t, st, "call-1"))
				requirements.NoError(err)
				assertions.Equal(prior, raw)
				messageID(t, st, "call-2")
				return
			}
			ev := loadEvidence(t, st, "call-1")
			if tc.priorSync {
				assertions.Contains(string(ev.Call), "detail summary", "the archived details outlive a refused read")
			}
			if tc.recorded {
				assertions.Equal(attachmentpolicy.StateStored, recordingRow(t, st, "call-1").State)
			}
			if tc.transcript != "" {
				assertions.Equal(tc.transcript, ev.Content.Transcript.Text)
				return
			}
			reason := "fetch_failed"
			if !tc.runFails {
				reason = "provider_refused"
			}
			assertions.Equal(reason, ev.Content.Transcript.Reason, "the transcript may exist on Bland")

			f.detailStatus, f.hookStatus, f.hook = 0, 0, retained
			_, err = imp.Import(t.Context(), o)
			requirements.NoError(err)
			ev = loadEvidence(t, st, "call-1")
			assertions.Equal("user: postcall speech", ev.Content.Transcript.Text)
			assertions.Equal("detail summary", ev.Content.Summary.Text)
		})
	}
}

// A recording the local attachment store can't write stops the run at once
// instead of reading as a fetch failure.
func TestUnusableAttachmentStoreStopsRun(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	record := `{"call_id":%q,"completed":true,"record":true,"recording_url":"https://example.invalid/audio","created_at":"2026-10-01T11:00:00Z","summary":"summary"}`
	f := &fixture{calls: map[string]string{"call-1": fmt.Sprintf(record, "call-1"), "call-2": fmt.Sprintf(record, "call-2")}, ids: []string{"call-1", "call-2"}, audio: "ID3synthetic-audio"}
	_, imp, o := setupImport(t, f)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	requirements.NoError(os.WriteFile(blocked, []byte("x"), 0o600))
	o.AttachmentsDir = blocked
	_, err := imp.Import(t.Context(), o)
	requirements.Error(err)
	assertions.NotContains(err.Error(), "bland call call-1")
	assertions.Zero(f.detailRequests["call-2"], "the run stops before the next call")
}
