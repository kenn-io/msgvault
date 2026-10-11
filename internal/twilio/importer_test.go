package twilio

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/callsync"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type fakePage struct {
	recordings []Recording
	next       string
}

type importSource struct {
	pages      map[string]fakePage
	calls      map[string]Call
	evidence   Evidence
	audioErr   error
	audioErrs  map[string]error
	audioBytes []byte
	// recordingsErr makes the per-call recordings list fail.
	recordingsErr error
	// transcriptsErr makes Transcripts fail.
	transcriptsErr error
	callErr        error
	pageCalls      []string
	getCalls       []string
	audioCalls     int
}

func (f *importSource) ListRecordingsPage(_ context.Context, after time.Time, _ int, cursor string) ([]Recording, string, error) {
	f.pageCalls = append(f.pageCalls, cursor)
	page, ok := f.pages[cursor]
	if !ok {
		return nil, "", fmt.Errorf("unexpected recording page cursor %q", cursor)
	}
	// Twilio filters DateCreated> by UTC date.
	after = after.UTC().Truncate(24 * time.Hour)
	var listed []Recording
	for _, r := range page.recordings {
		if !ParseTime(r.DateCreated).Before(after) {
			listed = append(listed, fromAPI(r))
		}
	}
	return listed, page.next, nil
}

func (f *importSource) GetCall(_ context.Context, id string) (Call, error) {
	f.getCalls = append(f.getCalls, id)
	if f.callErr != nil {
		return Call{}, f.callErr
	}
	c, ok := f.calls[id]
	if !ok {
		return Call{}, &APIError{Service: "voice", StatusCode: http.StatusNotFound}
	}
	return c, nil
}

func (f *importSource) CallRecordings(_ context.Context, id string) ([]Recording, error) {
	if f.recordingsErr != nil {
		return nil, f.recordingsErr
	}
	var out []Recording
	for _, page := range f.pages {
		for _, r := range page.recordings {
			if r.CallSID == id {
				out = append(out, fromAPI(r))
			}
		}
	}
	return out, nil
}

func (f *importSource) Transcripts(context.Context, []Recording) (Evidence, error) {
	return f.evidence, f.transcriptsErr
}

func (f *importSource) OpenRecording(_ context.Context, r Recording, _ int64) (io.ReadCloser, error) {
	f.audioCalls++
	if f.audioErr != nil {
		return nil, f.audioErr
	}
	if err := f.audioErrs[r.SID]; err != nil {
		return nil, err
	}
	if f.audioBytes != nil {
		return io.NopCloser(bytes.NewReader(f.audioBytes)), nil
	}
	return io.NopCloser(bytes.NewReader(testWAV)), nil
}

// fromAPI gives a recording the provider payload the client decodes with it.
func fromAPI(r Recording) Recording {
	raw, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	var out Recording
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}

func sid(prefix string, c byte) string { return prefix + strings.Repeat(string(c), 32) }

func testCall(id string) Call {
	return Call{SID: id, AccountSID: testAC, From: "+12025550101", To: "+12025550102", Status: "completed", StartTime: "2026-10-03T10:00:00Z", EndTime: "2026-10-03T10:01:00Z", Duration: "60"}
}

func testRecording(id, callID string) Recording {
	return Recording{SID: id, AccountSID: testAC, CallSID: callID, Status: "completed", DateCreated: "2026-10-03T10:00:00Z", Duration: "60", Channels: 2}
}

func fixtureImporter(t *testing.T) (*store.Store, *importSource, *Importer, ImportOptions) {
	t.Helper()
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(t, err)
	f := &importSource{
		calls: map[string]Call{testCA: testCall(testCA)},
		pages: map[string]fakePage{"": {recordings: []Recording{testRecording(testRE, testCA)}}},
	}
	imp := NewImporter(st, f)
	imp.now = func() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) }
	return st, f, imp, ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{MaxBytes: attachmentpolicy.DefaultChatMaxBytes}}
}

func loadState(t *testing.T, st *store.Store) callsync.State {
	t.Helper()
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	require.NoError(t, err)
	var state callsync.State
	require.NoError(t, json.Unmarshal([]byte(source.SyncCursor.String), &state))
	return state
}

func onlyAttachment(t *testing.T, st *store.Store) store.AttachmentRef {
	t.Helper()
	messages, _, err := st.ListMessages(0, 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	rows, err := st.MessageProviderAttachments(messages[0].ID, "twilio:")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	for _, row := range rows {
		return row
	}
	return store.AttachmentRef{}
}

// A recording that stored before a sibling failed must still count on the message.
func TestImporterPartialRecordingFailureUpdatesAttachmentStats(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, opts := fixtureImporter(t)
	failing := sid("RE", 'b')
	f.pages[""] = fakePage{recordings: []Recording{testRecording(testRE, testCA), testRecording(failing, testCA)}}
	f.audioErrs = map[string]error{failing: &APIError{Service: "recording media", StatusCode: http.StatusServiceUnavailable}}
	_, err := imp.Import(t.Context(), opts)
	require.Error(err)
	var attached bool
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT has_attachments, attachment_count FROM messages WHERE id = ?"), onlyMessageID(t, st)).Scan(&attached, &count))
	assert.True(attached)
	assert.Equal(2, count)
}

func TestImporterLimitedRunsResumeDiscovery(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, opts := fixtureImporter(t)
	second, third := sid("CA", 'b'), sid("CA", 'c')
	f.calls[second], f.calls[third] = testCall(second), testCall(third)
	f.pages = map[string]fakePage{
		"":      {recordings: []Recording{testRecording(testRE, testCA), testRecording(sid("RE", 'b'), second)}, next: "page2"},
		"page2": {recordings: []Recording{testRecording(sid("RE", 'c'), third)}},
	}
	imp.now = func() time.Time { return time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) }
	opts.Limit = 1
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.True(sum.DiscoveryPending)
	assert.EqualValues(1, sum.MeetingsAdded)
	assert.True(loadState(t, st).Watermark.IsZero(), "an unfinished traversal keeps the watermark")

	sum, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.True(sum.DiscoveryPending)
	assert.EqualValues(1, sum.MeetingsAdded)
	assert.Equal("page2", loadState(t, st).Incremental.Cursor, "the saved cursor names the page being consumed")

	imp.now = func() time.Time { return time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC) }
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, sum.MeetingsAdded)
	assert.EqualValues(1, sum.MeetingsProcessed)
	assert.Equal([]string{"", "", "page2", "page2"}, f.pageCalls)
	assert.Equal([]string{testCA, second, third}, f.getCalls, "each call is processed once")
	state := loadState(t, st)
	assert.Equal(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), state.Watermark, "the watermark is the traversal start")
	assert.Empty(state.Incremental.Cursor)
}

type failingCall struct {
	*importSource

	fail   string
	cancel context.CancelFunc
}

func (f *failingCall) GetCall(ctx context.Context, id string) (Call, error) {
	if id == f.fail {
		f.getCalls = append(f.getCalls, id)
		if f.cancel != nil {
			f.cancel()
			return Call{}, ctx.Err()
		}
		return Call{}, &APIError{Service: "voice", StatusCode: http.StatusInternalServerError}
	}
	return f.importSource.GetCall(ctx, id)
}

func TestImporterRetainedRecordingSurvivesMissingCallMetadata(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, opts := fixtureImporter(t)
	delete(f.calls, testCA)
	f.evidence = Evidence{Transcripts: []Transcript{{Kind: "legacy", ID: "TR" + strings.Repeat("a", 32), SourceID: testRE, Complete: true, Usable: true, Segments: []Segment{{Text: "Retained speech", Scope: testRE}}}}}
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, sum.MeetingsAdded)
	assert.EqualValues(1, sum.AttachmentsStored)
	assert.Contains(sum.Diagnostics, "call_metadata_unavailable")
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	require.NoError(err)
	archive, err := loadArchive(t.Context(), st, source.ID, testCA)
	require.NoError(err)
	assert.Empty(archive.Call.From)
	assert.Empty(archive.Call.Raw, "no fabricated provider Call response")
	assert.Equal(testAC, archive.Call.AccountSID)

	// A full refresh revives metadata on the same message and keeps its audio.
	f.calls[testCA] = testCall(testCA)
	opts.Full = true
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Zero(sum.MeetingsAdded)
	assert.EqualValues(1, sum.MeetingsUpdated)
	archive, err = loadArchive(t.Context(), st, source.ID, testCA)
	require.NoError(err)
	assert.Equal("+12025550101", archive.Call.From)

	// A later deletion keeps the previous metadata.
	delete(f.calls, testCA)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	archive, err = loadArchive(t.Context(), st, source.ID, testCA)
	require.NoError(err)
	assert.Equal("+12025550101", archive.Call.From)
	assert.Equal(1, f.audioCalls)
	message, err := st.GetMessage(onlyMessageID(t, st))
	require.NoError(err)
	assert.True(message.HasAttachments)
	assert.Contains(message.Body, "Retained speech")
}

func onlyMessageID(t *testing.T, st *store.Store) int64 {
	t.Helper()
	messages, _, err := st.ListMessages(0, 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	return messages[0].ID
}

func TestImporterSizeCapSkipWaitsForFullRetry(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, opts := fixtureImporter(t)
	opts.MediaPolicy = attachmentpolicy.Policy{MaxBytes: 1 << 20}
	f.audioBytes = make([]byte, 2<<20)
	copy(f.audioBytes, "RIFF\x24\x00\x00\x00WAVEfmt ")
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	require.Equal(1, f.audioCalls)
	assert.Equal(attachmentpolicy.SkipSizeCap, onlyAttachment(t, st).SkipReason)
	assert.Contains(sum.Diagnostics, "recording "+testRE+" skipped: larger than the 1 MiB cap; raise max_media_mb and run sync-twilio --full")

	imp.now = func() time.Time { return time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC) }
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(1, f.audioCalls, "an ordinary sync must not redownload a size-capped recording")

	opts.Full = true
	opts.MediaPolicy = attachmentpolicy.Policy{MaxBytes: 4 << 20}
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(2, f.audioCalls, "a full sync reconsiders a size-capped recording")
	assert.Equal(attachmentpolicy.StateStored, onlyAttachment(t, st).State)
}

func TestImporterMediaPolicySkipsWithoutFetching(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, opts := fixtureImporter(t)
	opts.MediaPolicy.Scope = attachmentpolicy.ScopeNone
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Zero(sum.AttachmentsStored)
	assert.Zero(f.audioCalls)
	assert.Equal(attachmentpolicy.SkipPolicyScope, onlyAttachment(t, st).SkipReason)
}

// --after bounds new recordings found through the call, not only the listing,
// and a completed --after run sets the watermark like any other.
func TestImporterAfterSkipsOlderRecordingsOfTheCall(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, opts := fixtureImporter(t)
	before := testRecording(sid("RE", 'b'), testCA)
	before.DateCreated = "2026-10-02T23:59:00Z"
	f.pages[""] = fakePage{recordings: []Recording{before, testRecording(testRE, testCA)}}
	opts.CreatedAfter = time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	rows, err := st.MessageProviderAttachments(onlyMessageID(t, st), "twilio:")
	require.NoError(err)
	assert.Len(rows, 1)
	assert.Contains(rows, "twilio:"+testRE)
	assert.Equal(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), loadState(t, st).Watermark)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	require.NoError(err)
	assert.True(source.LastSyncAt.Valid)
}

// A full traversal resumes after a scheduled incremental run in between.
func TestImporterFullTraversalSurvivesIncrementalRun(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	_, f, imp, opts := fixtureImporter(t)
	second := sid("CA", 'b')
	f.calls[second] = testCall(second)
	f.pages[""] = fakePage{recordings: []Recording{testRecording(testRE, testCA), testRecording(sid("RE", 'b'), second)}}
	imp.now = func() time.Time { return time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) }
	full := opts
	full.Full, full.Limit = true, 1
	sum, err := imp.Import(t.Context(), full)
	require.NoError(err)
	require.True(sum.DiscoveryPending)

	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)

	f.getCalls = nil
	_, err = imp.Import(t.Context(), full)
	require.NoError(err)
	assert.Equal([]string{second}, f.getCalls, "the resumed full traversal handles call 2")
}

// A local write failure stops the run and writes no row, so the next run retries.
func TestImporterLocalStoreFailureStopsRun(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, opts := fixtureImporter(t)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(os.WriteFile(blocked, []byte("x"), 0o600))
	good := opts.AttachmentsDir
	opts.AttachmentsDir = blocked
	_, err := imp.Import(t.Context(), opts)
	require.Error(err)
	rows, err := st.MessageProviderAttachments(onlyMessageID(t, st), "twilio:")
	require.NoError(err)
	assert.Empty(rows, "no failed row is written for a local failure")

	opts.AttachmentsDir = good
	f.audioErr = &APIError{Service: "recording media", StatusCode: http.StatusServiceUnavailable}
	sum, err := imp.Import(t.Context(), opts)
	require.Error(err)
	assert.EqualValues(1, sum.MeetingsUpdated, "the new pending row counts as a write, so caches refresh")
	assert.Equal(attachmentpolicy.StatePending, onlyAttachment(t, st).State)

	f.audioErr = nil
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, sum.AttachmentsStored)
	assert.Equal(attachmentpolicy.StateStored, onlyAttachment(t, st).State)
	assert.Equal(3, f.audioCalls)
}

// A recording Twilio refuses to serve becomes a failed row without failing
// the run, is reported once, and is fetched again while its call is relisted.
func TestImporterPermanentMediaFailure(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	var audioRequests atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2010-04-01/Accounts/" + testAC + "/Recordings.json":
			writeJSON(t, w, map[string]any{"recordings": []any{map[string]any{"sid": testRE, "account_sid": testAC, "call_sid": testCA, "status": "completed", "channels": 1}}})
		case "/2010-04-01/Accounts/" + testAC + "/Calls/" + testCA + ".json":
			writeJSON(t, w, map[string]any{"sid": testCA, "account_sid": testAC, "start_time": "2026-10-03T10:00:00Z", "end_time": "2026-10-03T10:01:00Z", "duration": "60"})
		case "/2010-04-01/Accounts/" + testAC + "/Calls/" + testCA + "/Recordings.json":
			assert.Equal("true", r.URL.Query().Get("IncludeSoftDeleted"))
			writeJSON(t, w, map[string]any{"recordings": []any{}})
		case "/2010-04-01/Accounts/" + testAC + "/Recordings/" + testRE + "/Transcriptions.json":
			writeJSON(t, w, map[string]any{"transcriptions": []any{}})
		case "/v2/Transcripts":
			writeJSON(t, w, map[string]any{"transcripts": []any{}})
		case "/2010-04-01/Accounts/" + testAC + "/Recordings/" + testRE + ".wav":
			if audioRequests.Add(1) == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			http.Redirect(w, r, "http://storage.invalid/audio.wav", http.StatusFound)
		default:
			assert.Fail("unexpected endpoint", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}, nil)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	imp := NewImporter(st, client)
	imp.now = func() time.Time { return now }
	opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{MaxBytes: attachmentpolicy.DefaultChatMaxBytes}}

	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err, "a 403 recording must not fail the run")
	assert.EqualValues(1, sum.Errors)
	row := onlyAttachment(t, st)
	assert.Equal(attachmentpolicy.StateFailed, row.State)
	assert.Equal(attachmentpolicy.SkipFetchFailure, row.SkipReason)

	now = now.Add(7 * time.Hour)
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(err, "a refused redirect must not fail the run")
	assert.Zero(sum.Errors, "a recording that keeps failing is reported once")
	assert.EqualValues(2, audioRequests.Load())
	assert.Equal(attachmentpolicy.StateFailed, onlyAttachment(t, st).State)
}

// Failed calls don't hold the listing watermark back, but remain retryable.
func TestImporterFailedCallDoesNotHoldWatermark(t *testing.T) {
	at := func(day, hour int) func() time.Time {
		return func() time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, time.UTC) }
	}
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, st *store.Store, f *importSource, imp *Importer, opts ImportOptions)
	}{
		{"recent call is retried by the next relist", func(t *testing.T, st *store.Store, f *importSource, imp *Importer, opts ImportOptions) {
			t.Helper()
			assert, require := assert.New(t), require.New(t)
			second := sid("CA", 'b')
			f.calls[second] = testCall(second)
			f.pages[""] = fakePage{recordings: []Recording{testRecording(testRE, testCA), testRecording(sid("RE", 'b'), second)}}
			imp.client = &failingCall{importSource: f, fail: second}
			_, err := imp.Import(t.Context(), opts)
			require.Error(err)
			assert.Equal(at(3, 12)(), loadState(t, st).Watermark)

			imp.client, imp.now = f, at(4, 12)
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			messages, _, err := st.ListMessages(0, 10)
			require.NoError(err)
			assert.Len(messages, 2)
			assert.Equal(at(4, 12)(), loadState(t, st).Watermark)
		}},
		{"old call failing in a first sync is retried", func(t *testing.T, st *store.Store, f *importSource, imp *Importer, opts ImportOptions) {
			t.Helper()
			assert, require := assert.New(t), require.New(t)
			old := sid("CA", 'b')
			f.calls[old] = testCall(old)
			recording := testRecording(sid("RE", 'b'), old)
			recording.DateCreated = "2026-09-20T10:00:00Z"
			f.pages[""] = fakePage{recordings: []Recording{testRecording(testRE, testCA), recording}}
			imp.client = &failingCall{importSource: f, fail: old}
			_, err := imp.Import(t.Context(), opts)
			require.ErrorContains(err, old)
			assert.Equal(at(3, 12)(), loadState(t, st).Watermark)

			imp.client, imp.now, f.getCalls = f, at(4, 12), nil
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			assert.Contains(f.getCalls, old, "retry the failed call outside the listing window")
			messages, _, err := st.ListMessages(0, 10)
			require.NoError(err)
			assert.Len(messages, 2)
		}},
		{"malformed call details fail the call", func(t *testing.T, st *store.Store, f *importSource, imp *Importer, opts ImportOptions) {
			t.Helper()
			f.callErr = errors.New("twilio voice: invalid JSON payload")
			sum, err := imp.Import(t.Context(), opts)
			require.ErrorContains(t, err, testCA)
			archive, err := loadArchive(t.Context(), st, sum.SourceID, testCA)
			require.NoError(t, err)
			assert.Empty(t, archive.Call.Raw, "retain no malformed call response")
			require.Len(t, archive.Recordings, 1, "retain validated recording evidence for retry")
			assert.Equal(t, testRE, archive.Recordings[0].SID)
			assert.Zero(t, f.audioCalls)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, f, imp, opts := fixtureImporter(t)
			tc.run(t, st, f, imp, opts)
		})
	}
}

// Walk through the relist window: audio and a transcript that arrive after
// the first sync reach the meeting while the call is relisted, a call is
// relisted while Twilio's date-granular bound still reaches it, a recording
// Twilio never produces ends unavailable once the call ages out, and after
// that a new transcript isn't fetched.
func TestImporterRelistWindowWalk(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, opts := fixtureImporter(t)
	never := sid("RE", 'b')
	processing := testRecording(testRE, testCA)
	processing.Status = "processing"
	stuck := testRecording(never, testCA)
	stuck.Status = "processing"
	f.pages[""] = fakePage{recordings: []Recording{processing, stuck}}
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, sum.MeetingsAdded)
	assert.Zero(f.audioCalls)

	// Day 3: the audio and transcript arrive.
	f.pages[""] = fakePage{recordings: []Recording{testRecording(testRE, testCA), stuck}}
	transcript := Transcript{Kind: "classic", ID: testGT, SourceID: testRE, Status: "completed", Complete: true, Usable: true, Segments: []Segment{{Text: "Synthetic budget discussion", Scope: testRE}}}
	f.evidence = Evidence{Transcripts: []Transcript{transcript}}
	imp.now = func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) }
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	msg, err := st.GetMessage(onlyMessageID(t, st))
	require.NoError(err)
	assert.Contains(msg.Body, "Synthetic budget discussion")
	rows, err := st.MessageProviderAttachments(msg.ID, "twilio:")
	require.NoError(err)
	assert.Equal(attachmentpolicy.StateStored, rows["twilio:"+testRE].State)
	assert.Equal(attachmentpolicy.StatePending, rows["twilio:"+never].State)

	// Day 8, evening: the next listing sends Oct 3 as its date bound, so the
	// call stays listed, nothing ages out, and the unchanged pending row isn't
	// rewritten.
	revision, err := st.DerivedDataRevision()
	require.NoError(err)
	imp.now = func() time.Time { return time.Date(2026, 10, 10, 18, 0, 0, 0, time.UTC) }
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Empty(sum.Diagnostics)
	assert.Zero(sum.MeetingsUpdated)
	after, err := st.DerivedDataRevision()
	require.NoError(err)
	assert.Equal(revision, after)
	rows, err = st.MessageProviderAttachments(msg.ID, "twilio:")
	require.NoError(err)
	assert.Equal(attachmentpolicy.StatePending, rows["twilio:"+never].State)

	// Day 10: relisted through the date bound, then aged out of the window.
	f.getCalls = nil
	imp.now = func() time.Time { return time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC) }
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal([]string{testCA}, f.getCalls)
	assert.Equal([]string{"1 recording(s) no longer retried: the call is older than the 7-day relist window; run sync-twilio --full to retry"}, sum.Diagnostics)
	rows, err = st.MessageProviderAttachments(msg.ID, "twilio:")
	require.NoError(err)
	assert.Equal(attachmentpolicy.StateUnavailable, rows["twilio:"+never].State, "Twilio never produced it")
	assert.Equal(1, f.audioCalls, "stored audio isn't downloaded again")

	// Day 11: the call is outside the window, so a new transcript isn't fetched.
	f.evidence.Transcripts = append(f.evidence.Transcripts, Transcript{Kind: "legacy", ID: "TR" + strings.Repeat("a", 32), SourceID: testRE, Status: "completed", Complete: true, Usable: true, Segments: []Segment{{Text: "too late", Scope: testRE}}})
	f.getCalls = nil
	imp.now = func() time.Time { return time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC) }
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Empty(f.getCalls)
	msg, err = st.GetMessage(msg.ID)
	require.NoError(err)
	assert.NotContains(msg.Body, "too late")
}

// A call whose recordings span pages, with its per-call list gone, imports the
// later page's recording in the same run, and --limit counts it once.
func TestImporterRecordingOnLaterPageImports(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, opts := fixtureImporter(t)
	later := sid("RE", 'b')
	f.pages = map[string]fakePage{
		"":      {recordings: []Recording{testRecording(testRE, testCA)}, next: "page2"},
		"page2": {recordings: []Recording{testRecording(later, testCA)}},
	}
	f.recordingsErr = &APIError{Service: "voice", StatusCode: http.StatusNotFound}
	opts.Limit = 1
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, sum.MeetingsProcessed)
	assert.False(sum.DiscoveryPending)
	rows, err := st.MessageProviderAttachments(onlyMessageID(t, st), "twilio:")
	require.NoError(err)
	require.Len(rows, 2)
	assert.Equal(attachmentpolicy.StateStored, rows["twilio:"+testRE].State)
	assert.Equal(attachmentpolicy.StateStored, rows["twilio:"+later].State)
}

// An encrypted or deleted recording ends unavailable without a download, and
// its transcripts still reach the meeting.
func TestImporterUnavailableRecordingIsNotDownloaded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Recording)
	}{
		{"encrypted", func(r *Recording) {
			r.EncryptionDetails = map[string]any{"type": "rsa-aes", "public_key_sid": "CR" + strings.Repeat("a", 32)}
		}},
		{"deleted before its transcript arrived", func(r *Recording) { r.Status = "deleted" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, f, imp, opts := fixtureImporter(t)
			recording := testRecording(testRE, testCA)
			tc.change(&recording)
			f.pages[""] = fakePage{recordings: []Recording{recording}}
			f.evidence = Evidence{Transcripts: []Transcript{{Kind: "legacy", ID: "TR" + strings.Repeat("a", 32), SourceID: testRE, Status: "completed", Complete: true, Usable: true, Segments: []Segment{{Text: "Synthetic budget discussion", Scope: testRE}}}}}
			_, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			assert.Zero(f.audioCalls)
			row := onlyAttachment(t, st)
			assert.Equal(attachmentpolicy.StateUnavailable, row.State)
			assert.Equal(attachmentpolicy.SkipSourceUnavailable, row.SkipReason)
			msg, err := st.GetMessage(onlyMessageID(t, st))
			require.NoError(err)
			assert.Contains(msg.Body, "Synthetic budget discussion")
		})
	}
}

func TestImporterOldCallRecoversAfterProviderFailure(t *testing.T) {
	for _, failure := range []string{"call", "recordings", "transcripts", "audio"} {
		t.Run(failure, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, f, imp, opts := fixtureImporter(t)
			imp.now = func() time.Time { return time.Date(2026, 12, 3, 12, 0, 0, 0, time.UTC) }
			providerErr := &APIError{Service: failure, StatusCode: http.StatusServiceUnavailable}
			switch failure {
			case "call":
				f.callErr = providerErr
			case "recordings":
				f.recordingsErr = providerErr
			case "transcripts":
				f.transcriptsErr = providerErr
			case "audio":
				f.audioErr = providerErr
			}
			for range 2 {
				_, err := imp.Import(t.Context(), opts)
				require.ErrorContains(err, testCA, "keep reporting the failure outside the listing window")
				if failure == "audio" {
					assert.Equal(attachmentpolicy.StatePending, onlyAttachment(t, st).State)
				}
			}

			f.callErr, f.recordingsErr, f.transcriptsErr, f.audioErr = nil, nil, nil, nil
			f.evidence = Evidence{Transcripts: []Transcript{{Kind: "legacy", ID: sid("TR", 'a'), SourceID: testRE, Complete: true, Usable: true, Segments: []Segment{{Text: "Recovered speech", Scope: testRE}}}}}
			// A fresh importer must recover from saved state, without --full.
			restarted := NewImporter(st, f)
			restarted.now = imp.now
			_, err := restarted.Import(t.Context(), opts)
			require.NoError(err)
			row := onlyAttachment(t, st)
			assert.Equal(attachmentpolicy.StateStored, row.State)
			data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, row.StoragePath))
			require.NoError(err)
			assert.Equal(testWAV, data)
			msg, err := st.GetMessage(onlyMessageID(t, st))
			require.NoError(err)
			assert.Contains(msg.Body, "Recovered speech")

			f.getCalls = nil
			_, err = restarted.Import(t.Context(), opts)
			require.NoError(err)
			assert.Empty(f.getCalls, "successful old calls leave the retry queue")
		})
	}
}

func TestImporterRetryRetainsDiscoveredRecording(t *testing.T) {
	for _, failure := range []string{"call", "recordings"} {
		t.Run(failure, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			var retrying atomic.Bool
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/2010-04-01/Accounts/" + testAC + "/Recordings.json":
					recordings := []Recording{testRecording(testRE, testCA)}
					if retrying.Load() {
						assert.Equal("2026-11-26", r.URL.Query().Get("DateCreated>"))
						recordings = []Recording{} // The old recording is outside the incremental window.
					}
					writeJSON(t, w, map[string]any{"recordings": recordings})
				case "/2010-04-01/Accounts/" + testAC + "/Calls/" + testCA + ".json":
					if !retrying.Load() && failure == "call" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					w.WriteHeader(http.StatusNotFound)
				case "/2010-04-01/Accounts/" + testAC + "/Calls/" + testCA + "/Recordings.json":
					if !retrying.Load() && failure == "recordings" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					w.WriteHeader(http.StatusNotFound)
				case "/2010-04-01/Accounts/" + testAC + "/Recordings/" + testRE + "/Transcriptions.json":
					writeJSON(t, w, map[string]any{"transcriptions": []any{map[string]any{
						"sid": sid("TR", 'a'), "account_sid": testAC, "recording_sid": testRE,
						"status": "completed", "transcription_text": "Recovered speech",
					}}})
				case "/v2/Transcripts":
					writeJSON(t, w, map[string]any{"transcripts": []any{}})
				case "/2010-04-01/Accounts/" + testAC + "/Recordings/" + testRE + ".wav":
					_, err := w.Write(testWAV)
					assert.NoError(err)
				default:
					assert.Fail("unexpected endpoint", r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}, nil)
			st, _, imp, opts := fixtureImporter(t)
			imp.client = client
			imp.now = func() time.Time { return time.Date(2026, 12, 3, 12, 0, 0, 0, time.UTC) }
			_, err := imp.Import(t.Context(), opts)
			require.Error(err)
			require.Equal([]string{testCA}, loadState(t, st).RetryIDs)

			// Both per-call endpoints are now gone, but recording-scoped media
			// and transcripts remain available. A restart must retain their IDs.
			retrying.Store(true)
			restarted := NewImporter(st, client)
			restarted.now = imp.now
			_, err = restarted.Import(t.Context(), opts)
			require.NoError(err)
			row := onlyAttachment(t, st)
			assert.Equal(attachmentpolicy.StateStored, row.State)
			data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, row.StoragePath))
			require.NoError(err)
			assert.Equal(testWAV, data)
			msg, err := st.GetMessage(onlyMessageID(t, st))
			require.NoError(err)
			assert.Contains(msg.Body, "Recovered speech")
			assert.Empty(loadState(t, st).RetryIDs)
		})
	}
}

func TestImporterBoundedFullSyncPreservesIncrementalCoverage(t *testing.T) {
	for _, after := range []time.Time{
		time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC),  // Covers the old incremental window.
		time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC),  // Leaves part of that window unlisted.
		time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), // Skips calls during the outage.
	} {
		t.Run(after.Format(time.DateOnly), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, f, imp, opts := fixtureImporter(t)
			_, err := imp.Import(t.Context(), opts)
			require.NoError(err)

			missed := sid("CA", 'b')
			call := testCall(missed)
			call.StartTime = "2026-10-05T10:00:00Z"
			f.calls[missed] = call
			recording := testRecording(sid("RE", 'b'), missed)
			recording.DateCreated = call.StartTime
			f.pages[""] = fakePage{recordings: []Recording{testRecording(testRE, testCA), recording}}
			imp.now = func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }
			full := opts
			full.Full, full.CreatedAfter = true, after
			_, err = imp.Import(t.Context(), full)
			require.NoError(err)
			want := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
			if after.Day() == 26 {
				want = imp.now()
			}
			assert.Equal(want, loadState(t, st).Watermark)

			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			messages, _, err := st.ListMessages(0, 10)
			require.NoError(err)
			assert.Len(messages, 2, "incremental sync must still reach calls from the outage")
		})
	}
}

func TestImporterQueuedRetriesRespectLimitAndSurviveCancellation(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, opts := fixtureImporter(t)
	second, unfinished := sid("CA", 'b'), sid("CA", 'c')
	f.calls[second], f.calls[unfinished] = testCall(second), testCall(unfinished)
	waiting := testRecording(sid("RE", 'c'), unfinished)
	waiting.Status = "processing"
	f.pages[""] = fakePage{recordings: []Recording{testRecording(testRE, testCA), testRecording(sid("RE", 'b'), second), waiting}}
	imp.now = func() time.Time { return time.Date(2026, 12, 3, 12, 0, 0, 0, time.UTC) }
	f.audioErr = &APIError{Service: "media", StatusCode: http.StatusServiceUnavailable}
	_, err := imp.Import(t.Context(), opts)
	require.Error(err)
	assert.Equal([]string{testCA, second}, loadState(t, st).RetryIDs)
	messages, _, err := st.ListMessages(0, 10)
	require.NoError(err)
	for _, msg := range messages {
		rows, err := st.MessageProviderAttachments(msg.ID, "twilio:")
		require.NoError(err)
		for _, row := range rows {
			want := attachmentpolicy.StatePending
			if msg.SourceMessageID == unfinished {
				want = attachmentpolicy.StateUnavailable
			}
			assert.Equal(want, row.State, "only queued calls are excluded from aging")
		}
	}

	opts.Limit = 1
	sum, err := imp.Import(t.Context(), opts)
	require.Error(err)
	assert.EqualValues(1, sum.MeetingsProcessed)
	assert.True(sum.DiscoveryPending)
	assert.Equal([]string{second, testCA}, loadState(t, st).RetryIDs, "rotate a persistent failure behind unattempted retries")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	imp.client = &failingCall{importSource: f, fail: second, cancel: cancel}
	_, err = imp.Import(ctx, opts)
	require.ErrorIs(err, context.Canceled)
	assert.Equal([]string{second, testCA}, loadState(t, st).RetryIDs)

	imp.client, f.audioErr = f, nil
	for _, remaining := range [][]string{{testCA}, nil} {
		sum, err = imp.Import(t.Context(), opts)
		require.NoError(err)
		assert.EqualValues(1, sum.AttachmentsStored)
		assert.EqualValues(1, sum.MeetingsProcessed)
		assert.Equal(remaining, loadState(t, st).RetryIDs)
	}
}

func TestImporterBoundedSyncKeepsUnresolvedRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		relisted bool
	}{
		{"outside listing", false},
		{"relisted with newer recording", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, f, imp, opts := fixtureImporter(t)
			imp.now = func() time.Time { return time.Date(2026, 12, 3, 12, 0, 0, 0, time.UTC) }
			f.audioErr = &APIError{Service: "media", StatusCode: http.StatusServiceUnavailable}
			_, err := imp.Import(t.Context(), opts)
			require.Error(err)
			f.audioErr = nil
			if tc.relisted {
				newer := testRecording(sid("RE", 'b'), testCA)
				newer.DateCreated = "2026-12-02T10:00:00Z"
				f.pages[""] = fakePage{recordings: []Recording{testRecording(testRE, testCA), newer}}
			}

			full := opts
			full.Full = true
			full.CreatedAfter = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
			_, err = imp.Import(t.Context(), full)
			require.NoError(err)
			assert.Equal([]string{testCA}, loadState(t, st).RetryIDs)
			rows, err := st.MessageProviderAttachments(onlyMessageID(t, st), "twilio:")
			require.NoError(err)
			assert.Equal(attachmentpolicy.StatePending, rows["twilio:"+testRE].State)

			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			rows, err = st.MessageProviderAttachments(onlyMessageID(t, st), "twilio:")
			require.NoError(err)
			assert.Equal(attachmentpolicy.StateStored, rows["twilio:"+testRE].State)
			assert.Empty(loadState(t, st).RetryIDs)
		})
	}
}

func TestImporterLimitedDiscoveryProgressesWithPersistentRetry(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, opts := fixtureImporter(t)
	second := sid("CA", 'b')
	f.calls[second] = testCall(second)
	f.pages[""] = fakePage{recordings: []Recording{testRecording(testRE, testCA), testRecording(sid("RE", 'b'), second)}}
	imp.client = &failingCall{importSource: f, fail: testCA}
	opts.Limit = 1
	_, err := imp.Import(t.Context(), opts)
	require.Error(err)

	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, sum.MeetingsAdded)
	assert.EqualValues(1, sum.MeetingsProcessed)
	assert.True(sum.DiscoveryPending, "the failed first call still needs a retry")
	messages, err := st.MessageMetadataBatch(sum.SourceID, []string{second})
	require.NoError(err)
	require.Contains(messages, second, "discovery reaches the next call despite the queued retry")
	msg, err := st.GetMessage(messages[second].ID)
	require.NoError(err)
	assert.Equal(second, msg.SourceMessageID)
	assert.True(msg.HasAttachments)
	assert.Equal([]string{testCA}, loadState(t, st).RetryIDs)
}
