package twilio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type importSource struct {
	recordings                []Recording
	calls                     map[string]Call
	listedCalls               []Call
	relayDiscovery            bool
	recordingListCalls        int
	listRecordingCalls        int
	recordingPages            map[string][]Recording
	recordingPageNext         map[string]string
	recordingPageCalls        []string
	recordingPageSizes        []int
	recordingPageAfters       []time.Time
	filterRecordingPagesAfter bool
	callPages                 map[string][]Call
	callPageNext              map[string]string
	callPageCalls             []string
	callPageSizes             []int
	listCallCalls             int
	evidence                  Evidence
	transcriptReader          func(context.Context, Call, []Recording) (Evidence, error)
	listErr                   error
	audioErr                  error
	callErr                   error
	recordingErr              error
	audioCalls                int
	onGetCall                 func(string)
}

func (f *importSource) ListRecordings(context.Context, time.Time) ([]Recording, error) {
	f.listRecordingCalls++
	return f.recordings, f.listErr
}
func (f *importSource) ListRecordingsPage(_ context.Context, after time.Time, pageSize int, cursor string) ([]Recording, string, error) {
	f.recordingPageCalls = append(f.recordingPageCalls, cursor)
	f.recordingPageSizes = append(f.recordingPageSizes, pageSize)
	f.recordingPageAfters = append(f.recordingPageAfters, after)
	if f.recordingPages == nil {
		return f.recordings, "", f.listErr
	}
	page, ok := f.recordingPages[cursor]
	if !ok {
		return nil, "", fmt.Errorf("unexpected recording page cursor %q", cursor)
	}
	if f.filterRecordingPagesAfter && !after.IsZero() {
		utc := after.UTC()
		cutoff := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
		filtered := make([]Recording, 0, len(page))
		for _, recording := range page {
			created := ParseTime(recording.DateCreated)
			if !created.IsZero() && !created.Before(cutoff) {
				filtered = append(filtered, recording)
			}
		}
		page = filtered
	}
	return page, f.recordingPageNext[cursor], nil
}
func (f *importSource) ListCalls(context.Context, time.Time) ([]Call, error) {
	f.listCallCalls++
	return f.listedCalls, nil
}
func (f *importSource) ListCallsPage(_ context.Context, _ time.Time, pageSize int, cursor string) ([]Call, string, error) {
	f.callPageCalls = append(f.callPageCalls, cursor)
	f.callPageSizes = append(f.callPageSizes, pageSize)
	if f.callPages == nil {
		return f.listedCalls, "", nil
	}
	page, ok := f.callPages[cursor]
	if !ok {
		return nil, "", fmt.Errorf("unexpected calls page cursor %q", cursor)
	}
	return page, f.callPageNext[cursor], nil
}
func (f *importSource) DiscoverCalls() bool { return f.relayDiscovery }
func (f *importSource) GetCall(_ context.Context, id string) (Call, error) {
	if f.onGetCall != nil {
		f.onGetCall(id)
	}
	if f.callErr != nil {
		return Call{}, f.callErr
	}
	c, ok := f.calls[id]
	if !ok {
		return Call{}, errors.New("temporary call retrieval failure")
	}
	return c, nil
}
func (f *importSource) CallRecordings(_ context.Context, id string) ([]Recording, error) {
	f.recordingListCalls++
	if f.recordingErr != nil {
		return nil, f.recordingErr
	}
	var out []Recording
	for _, r := range f.recordings {
		if r.CallSID == id {
			out = append(out, r)
		}
	}
	return out, nil
}

func TestImporterExpiredMetadataFailuresRemainRetryable(t *testing.T) {
	for _, code := range []int{403, 500} {
		for _, endpoint := range []string{"call", "recordings"} {
			t.Run(fmt.Sprintf("%s-%d", endpoint, code), func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				st, source, importer, opts := fixtureImporter(t)
				opts.MediaPolicy = attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone}
				failure := &APIError{Service: "voice", StatusCode: code}
				if endpoint == "call" {
					source.callErr = failure
				} else {
					source.recordingErr = failure
				}
				_, err := importer.Import(t.Context(), opts)
				require.Error(err)
				later := importer.now().Add(30 * 24 * time.Hour)
				importer.now = func() time.Time { return later }
				summary, err := importer.Import(t.Context(), opts)
				require.Error(err)
				assert.EqualValues(1, summary.MaintenanceRetries)
				state, err := loadState(st, summary.SourceID)
				require.NoError(err)
				known := state.Known[testCA]
				assert.True(known.Failed)
				assert.True(known.Unverified)
				assert.False(known.NextAttempt.IsZero())
				assert.False(known.CallMetadataUnavailable)
				assert.False(known.RecordingsUnavailable)
				assert.True(state.Watermark.IsZero())
			})
		}
	}
}

func TestImporterFinalDueAttemptTerminalizesAbsentMedia(t *testing.T) {
	for _, status := range []string{"completed", "in-progress"} {
		t.Run(status, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, source, importer, opts := fixtureImporter(t)
			source.recordings[0].Status = status
			source.audioErr = &APIError{Service: "voice", StatusCode: 404}
			_, err := importer.Import(t.Context(), opts)
			require.NoError(err)
			later := importer.now().Add(8 * 24 * time.Hour)
			importer.now = func() time.Time { return later }
			summary, err := importer.Import(t.Context(), opts)
			require.NoError(err)
			assert.EqualValues(1, summary.MaintenanceRetries)
			state, err := loadState(st, summary.SourceID)
			require.NoError(err)
			assert.True(state.Known[testCA].NextAttempt.IsZero())
			assert.False(state.Known[testCA].Failed)
			messages, _, err := st.ListMessages(0, 10)
			require.NoError(err)
			require.Len(messages, 1)
			var attachmentState string
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT attachment_state FROM attachments WHERE message_id = ?"), messages[0].ID).Scan(&attachmentState))
			assert.Equal(string(attachmentpolicy.StateUnavailable), attachmentState)
		})
	}
}

func (f *importSource) Transcripts(ctx context.Context, call Call, recordings []Recording) (Evidence, error) {
	if f.transcriptReader != nil {
		return f.transcriptReader(ctx, call, recordings)
	}
	return f.evidence, nil
}
func (f *importSource) OpenRecording(context.Context, Recording, int64) (io.ReadCloser, error) {
	f.audioCalls++
	if f.audioErr != nil {
		return nil, f.audioErr
	}
	return io.NopCloser(bytes.NewReader([]byte("RIFF\x24\x00\x00\x00WAVEfmt synthetic audio"))), nil
}

func fixtureImporter(t *testing.T) (*store.Store, *importSource, *Importer, ImportOptions) {
	t.Helper()
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(t, err)
	ac := "AC" + strings.Repeat("a", 32)
	ca := "CA" + strings.Repeat("a", 32)
	re := "RE" + strings.Repeat("a", 32)
	f := &importSource{calls: map[string]Call{ca: {SID: ca, AccountSID: ac, From: "+12025550101", To: "+12025550102", Status: "completed", StartTime: "2026-10-03T10:00:00Z", EndTime: "2026-10-03T10:01:00Z", Duration: "60"}}, recordings: []Recording{{SID: re, AccountSID: ac, CallSID: ca, Status: "completed", DateCreated: "2026-10-03T10:00:00Z", Duration: "60", Channels: 2}}}
	imp := NewImporter(st, f)
	imp.now = func() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) }
	return st, f, imp, ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", AttachmentsDir: t.TempDir()}
}

func TestImporterCoverageDiagnosticsAreReturnedOnceForSummary(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	_, source, importer, opts := fixtureImporter(t)
	source.evidence.Diagnostics = []string{"regional_transcript_unavailable", "regional_transcript_unavailable"}
	var progress []string
	opts.Progress = func(message string) { progress = append(progress, message) }
	summary, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal([]string{"regional_transcript_unavailable"}, summary.Diagnostics)
	for _, message := range progress {
		assert.NotContains(message, "regional_transcript_unavailable", "coverage is printed by the CLI summary")
	}
}

func TestImporterFailedSyncEmitsCoverageDiagnostics(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	_, source, importer, opts := fixtureImporter(t)
	source.evidence.Diagnostics = []string{"regional_transcript_unavailable"}
	source.audioErr = errors.New("synthetic media failure")
	var progress []string
	opts.Progress = func(message string) { progress = append(progress, message) }
	summary, err := importer.Import(t.Context(), opts)
	require.Error(err)
	require.NotEmpty(summary.Diagnostics)
	want := make([]string, len(summary.Diagnostics))
	for index, diagnostic := range summary.Diagnostics {
		want[index] = "Coverage: " + diagnostic
	}
	assert.Equal(want, progress)
}

func TestImporterLaterTranscriptPreservesAudioAndOmissions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, opts := fixtureImporter(t)
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, sum.MeetingsAdded)
	assert.EqualValues(1, sum.AttachmentsStored)
	messages, total, err := st.ListMessages(0, 10)
	require.NoError(err)
	require.EqualValues(1, total)
	id := messages[0].ID
	msg, err := st.GetMessage(id)
	require.NoError(err)
	assert.False(msg.IsFromMe)
	require.Len(msg.Attachments, 1)
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	content := meetingcontent.Decode(RawFormat, raw, nil)
	assert.Equal(meetingcontent.StateUnavailable, content.Transcript.State)
	require.NotNil(content.DurationSeconds)
	assert.InDelta(60.0, *content.DurationSeconds, 1e-9)
	f.evidence = Evidence{Transcripts: []Transcript{{Kind: "classic", ID: "GT" + strings.Repeat("a", 32), SourceID: f.recordings[0].SID, Status: "completed", Complete: true, Usable: true, Segments: []Segment{{Speaker: "channel 1", Text: "Synthetic budget discussion", Scope: f.recordings[0].SID}}}}}
	imp.now = func() time.Time { return time.Date(2026, 10, 3, 19, 0, 0, 0, time.UTC) }
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, sum.MeetingsUpdated)
	f.evidence = Evidence{}
	f.recordings = nil
	opts.Full = true
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	msg, err = st.GetMessage(id)
	require.NoError(err)
	assert.Contains(msg.Body, "Synthetic budget discussion")
	assert.Len(msg.Attachments, 1)
	assert.Equal(1, f.audioCalls)
	_, total, err = st.ListMessages(0, 10)
	require.NoError(err)
	assert.EqualValues(1, total)
	results, n, err := st.SearchMessages("budget", 0, 10)
	require.NoError(err)
	assert.EqualValues(1, n)
	assert.Len(results, 1)
}
func TestImporterRetriesFailedBytesWithUnchangedEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, opts := fixtureImporter(t)
	f.audioErr = errors.New("temporary transport")
	sum, err := imp.Import(t.Context(), opts)
	require.Error(err)
	assert.EqualValues(1, sum.MeetingsAdded)
	f.audioErr = nil
	imp.now = func() time.Time { return time.Date(2026, 10, 3, 19, 0, 0, 0, time.UTC) }
	sum, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, sum.AttachmentsStored)
	assert.Equal(2, f.audioCalls)
	_, total, err := st.ListMessages(0, 10)
	require.NoError(err)
	assert.EqualValues(1, total)
}
func TestImporterLimitMakesProgressWithFailedNewestCall(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, opts := fixtureImporter(t)
	opts.Limit = 1
	broken := f.recordings[0]
	broken.SID = "RE" + strings.Repeat("b", 32)
	broken.CallSID = "CA" + strings.Repeat("b", 32)
	f.recordings = append([]Recording{broken}, f.recordings...)
	_, err := imp.Import(t.Context(), opts)
	require.Error(err)
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, sum.MeetingsAdded)
	_, total, err := st.ListMessages(0, 10)
	require.NoError(err)
	assert.EqualValues(1, total)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	require.NoError(err)
	state, err := loadState(st, source.ID)
	require.NoError(err)
	assert.True(state.Watermark.IsZero(), "failed discovered call must hold discovery checkpoint")
}

func TestImporterDueFailureDoesNotConsumeDiscoveryLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, opts := fixtureImporter(t)
	opts.Limit = 1
	broken := f.recordings[0]
	broken.SID = "RE" + strings.Repeat("b", 32)
	broken.CallSID = "CA" + strings.Repeat("b", 32)
	f.recordings = append([]Recording{broken}, f.recordings...)
	_, err := imp.Import(t.Context(), opts)
	require.Error(err)
	imp.now = func() time.Time { return time.Date(2026, 10, 3, 19, 0, 0, 0, time.UTC) }
	sum, err := imp.Import(t.Context(), opts)
	require.Error(err, "broken call must still be retried independently")
	assert.EqualValues(1, sum.MeetingsAdded)
	assert.EqualValues(1, sum.MaintenanceRetries)
	_, total, err := st.ListMessages(0, 10)
	require.NoError(err)
	assert.EqualValues(1, total, "new healthy call must receive the discovery budget")
}

func TestImporterLimitedDiscoveryPersistsRecordingsAcrossCallLookupFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, source, importer, opts := fixtureImporter(t)
	opts.Limit = 1
	opts.MediaPolicy = attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone}
	recording := source.recordings[0]
	source.recordingPages = map[string][]Recording{"": {recording}, "cursor-1": nil}
	source.recordingPageNext = map[string]string{"": "cursor-1", "cursor-1": ""}
	source.callErr = errors.New("temporary call retrieval failure")

	_, err := importer.Import(t.Context(), opts)
	require.Error(err)
	sourceRow, err := st.GetSourceByTypeAndIdentifier(SourceType, opts.Identifier)
	require.NoError(err)
	state, err := loadState(st, sourceRow.ID)
	require.NoError(err)
	assert.Empty(state.DiscoveryQueue, "failed call is removed from the discovery queue")
	assert.Equal("cursor-1", state.DiscoveryCursor, "the discovery cursor has already advanced past the recording")
	assert.False(state.DiscoveryDone)
	assert.True(state.Known[testCA].Failed)
	require.Len(state.Known[testCA].PendingRecordings, 1,
		"recording metadata must survive independently of the discovery cursor")
	assert.Equal(recording.SID, state.Known[testCA].PendingRecordings[0].SID)

	// The account-wide recording has disappeared by the time the due retry runs,
	// and the call recordings endpoint has also expired.
	source.callErr = nil
	source.recordings = nil
	source.recordingErr = &APIError{Service: "voice", StatusCode: 404}
	importer.now = func() time.Time { return time.Date(2026, 10, 3, 19, 0, 0, 0, time.UTC) }

	summary, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, summary.MeetingsAdded)
	assert.Equal([]string{"", "cursor-1"}, source.recordingPageCalls,
		"the due retry should continue from the advanced cursor after the recording disappears")
	messages, total, err := st.ListMessages(0, 10)
	require.NoError(err)
	require.EqualValues(1, total)
	message, err := st.GetMessage(messages[0].ID)
	require.NoError(err)
	require.Len(message.Attachments, 1, "retained recording metadata should create its archived attachment")
	assert.Equal(recording.SID+".wav", message.Attachments[0].Filename)
	state, err = loadState(st, summary.SourceID)
	require.NoError(err)
	assert.Empty(state.Known[testCA].PendingRecordings, "successfully archived metadata should leave retry state")
}

func TestImporterLimitedRunsResumePagedRecordingDiscovery(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, source, importer, opts := fixtureImporter(t)
	opts.Limit = 1
	opts.MediaPolicy = attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone}

	callA, callB, callC := "CA"+strings.Repeat("a", 32), "CA"+strings.Repeat("b", 32), "CA"+strings.Repeat("c", 32)
	recordingA1, recordingA2 := "RE"+strings.Repeat("a", 32), "RE"+strings.Repeat("b", 32)
	recordingB, recordingC := "RE"+strings.Repeat("c", 32), "RE"+strings.Repeat("d", 32)
	baseCall := source.calls[testCA]
	calls := map[string]Call{}
	for _, id := range []string{callA, callB, callC} {
		call := baseCall
		call.SID = id
		call.DateCreated = "2026-10-03T10:00:00Z"
		calls[id] = call
	}
	source.calls = calls
	source.recordings = []Recording{
		{SID: recordingA1, AccountSID: testAC, CallSID: callA, Status: "completed", DateCreated: "2026-10-03T10:00:00Z"},
		{SID: recordingA2, AccountSID: testAC, CallSID: callA, Status: "completed", DateCreated: "2026-10-03T10:01:00Z"},
		{SID: recordingB, AccountSID: testAC, CallSID: callB, Status: "completed", DateCreated: "2026-10-03T10:02:00Z"},
		{SID: recordingC, AccountSID: testAC, CallSID: callC, Status: "completed", DateCreated: "2026-10-03T10:03:00Z"},
	}
	source.recordingPages = map[string][]Recording{
		"":         {source.recordings[0], source.recordings[2]},
		"cursor-1": {source.recordings[1], source.recordings[3]},
	}
	source.recordingPageNext = map[string]string{"": "cursor-1", "cursor-1": ""}
	var attempted []string
	source.onGetCall = func(id string) { attempted = append(attempted, id) }

	for index, id := range []string{callA, callB, callC} {
		summary, err := importer.Import(t.Context(), opts)
		require.NoError(err)
		assert.EqualValues(1, summary.MeetingsProcessed)
		assert.EqualValues(1, summary.MeetingsAdded)
		assert.Equal(id, attempted[index])
		state, err := loadState(st, summary.SourceID)
		require.NoError(err)
		if index < 2 {
			assert.True(state.Watermark.IsZero(), "incomplete limited discovery must retain its watermark")
		} else {
			assert.False(state.Watermark.IsZero(), "complete discovery can advance its watermark")
		}
	}

	assert.Equal([]string{"", "cursor-1"}, source.recordingPageCalls, "the second limited run should drain queued calls before requesting another page")
	assert.Equal([]int{1000, 1000}, source.recordingPageSizes)
	assert.Zero(source.listRecordingCalls, "limited sync should use page reads instead of materializing the full collection")
	assert.Equal([]string{callA, callB, callC}, attempted, "a call split across pages should hydrate only once")
	_, count, err := st.ListMessages(0, 10)
	require.NoError(err)
	assert.EqualValues(3, count)
}

func TestLimitedDiscoveryWatermarkUsesTraversalStart(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, source, importer, opts := fixtureImporter(t)
	opts.Limit = 1
	opts.MediaPolicy = attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := start
	importer.now = func() time.Time { return now }

	callA, callB, callC := testCA, "CA"+strings.Repeat("b", 32), "CA"+strings.Repeat("c", 32)
	baseCall := source.calls[testCA]
	calls := map[string]Call{}
	for _, id := range []string{callA, callB, callC} {
		call := baseCall
		call.SID = id
		call.DateCreated = start.Add(15 * time.Minute).Format(time.RFC3339)
		calls[id] = call
	}
	source.calls = calls
	recordingA := source.recordings[0]
	recordingA.CallSID = callA
	recordingA.DateCreated = start.Add(15 * time.Minute).Format(time.RFC3339)
	recordingB := recordingA
	recordingB.SID = "RE" + strings.Repeat("b", 32)
	recordingB.CallSID = callB
	recordingB.DateCreated = start.Add(12 * time.Hour).Format(time.RFC3339)
	recordingC := recordingA
	recordingC.SID = "RE" + strings.Repeat("c", 32)
	recordingC.CallSID = callC
	recordingC.DateCreated = start.Add(3 * 24 * time.Hour).Format(time.RFC3339)
	source.recordings = []Recording{recordingA, recordingB, recordingC}
	source.recordingPages = map[string][]Recording{"": {recordingA}, "cursor-1": {recordingC}}
	source.recordingPageNext = map[string]string{"": "cursor-1", "cursor-1": ""}
	source.filterRecordingPagesAfter = true
	var attempted []string
	source.onGetCall = func(id string) { attempted = append(attempted, id) }

	first, err := importer.Import(t.Context(), opts)
	requirements.NoError(err)
	assertions.EqualValues(1, first.MeetingsProcessed)
	state, err := loadState(st, first.SourceID)
	requirements.NoError(err)
	assertions.True(state.Watermark.IsZero())

	// This call appears on an already-consumed first page while the saved
	// cursor points at the later page. The traversal now lasts longer than the
	// overlap used by the next discovery window.
	now = start.Add(8 * 24 * time.Hour)
	source.recordingPages[""] = []Recording{recordingA, recordingB}
	second, err := importer.Import(t.Context(), opts)
	requirements.NoError(err)
	assertions.EqualValues(1, second.MeetingsAdded)
	assertions.Contains(attempted, callC, "the saved cursor should reach the later page")
	state, err = loadState(st, second.SourceID)
	requirements.NoError(err)
	assertions.True(state.Watermark.Equal(start), "completed discovery must use its persisted traversal start")

	// A fresh traversal must still include the late call even though it is
	// older than the final run's seven-day overlap cutoff.
	opts.Limit = 10
	third, err := importer.Import(t.Context(), opts)
	requirements.NoError(err)
	assertions.EqualValues(1, third.MeetingsAdded)
	assertions.Contains(attempted, callB, "the next traversal must recover the call added behind the saved cursor")
	requirements.GreaterOrEqual(len(source.recordingPageAfters), 3)
	assertions.True(source.recordingPageAfters[2].Equal(start.Add(-7 * 24 * time.Hour)))
}

func TestImporterLimitedRunsResumePagedRelayDiscovery(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, source, importer, opts := fixtureImporter(t)
	opts.Limit = 1
	opts.MediaPolicy = attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone}
	source.relayDiscovery = true

	callA := testCA
	callB := "CAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	callC := "CAcccccccccccccccccccccccccccccccc"
	baseCall := source.calls[callA]
	calls := map[string]Call{}
	for _, id := range []string{callA, callB, callC} {
		call := baseCall
		call.SID = id
		calls[id] = call
	}
	source.calls = calls
	source.listedCalls = []Call{calls[callA], calls[callB], calls[callC]}
	source.callPages = map[string][]Call{
		"":         {calls[callA]},
		"cursor-1": {calls[callB]},
		"cursor-2": {calls[callC]},
	}
	source.callPageNext = map[string]string{"": "cursor-1", "cursor-1": "cursor-2", "cursor-2": ""}
	recordingA := source.recordings[0]
	source.recordings = []Recording{recordingA}
	source.recordingPages = map[string][]Recording{"": {recordingA}}
	source.recordingPageNext = map[string]string{"": ""}
	var attempted []string
	source.onGetCall = func(id string) { attempted = append(attempted, id) }

	for index, id := range []string{callA, callB, callC} {
		summary, err := importer.Import(t.Context(), opts)
		requirements.NoError(err)
		assertions.EqualValues(1, summary.MeetingsProcessed)
		assertions.EqualValues(1, summary.MeetingsAdded)
		assertions.Equal(id, attempted[index])
		state, err := loadState(st, summary.SourceID)
		requirements.NoError(err)
		if index < 2 {
			assertions.True(state.Watermark.IsZero(), "incomplete limited Relay discovery must retain its watermark")
		} else {
			assertions.False(state.Watermark.IsZero(), "completed limited Relay discovery can advance its watermark")
		}
	}

	assertions.Equal([]string{"", "cursor-1", "cursor-2"}, source.callPageCalls)
	assertions.Equal([]int{1000, 1000, 1000}, source.callPageSizes)
	assertions.Zero(source.listCallCalls, "limited Relay discovery must not materialize the full Calls collection")
	assertions.Equal([]string{callA, callB, callC}, attempted, "a Relay call also found through recordings must import once")
	_, count, err := st.ListMessages(0, 10)
	requirements.NoError(err)
	assertions.EqualValues(3, count)
}

func TestImporterLimitedRelayCursorSurvivesDiscoveryToggleAfterFailedRun(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, source, importer, opts := fixtureImporter(t)
	opts.Limit = 1
	opts.CreatedAfter = time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	opts.MediaPolicy = attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone}

	callA := testCA
	callB := "CAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	baseCall := source.calls[callA]
	baseCall.DateCreated = opts.CreatedAfter.Format(time.RFC3339)
	call := baseCall
	call.SID = callB
	source.calls[callA] = baseCall
	source.calls[callB] = call
	source.recordings = nil
	source.recordingPages = map[string][]Recording{"": nil}
	source.recordingPageNext = map[string]string{"": ""}
	source.callPages = map[string][]Call{"": {baseCall}, "cursor-1": {call}}
	source.callPageNext = map[string]string{"": "cursor-1", "cursor-1": "cursor-2"}
	source.relayDiscovery = true
	var attempted []string
	source.onGetCall = func(id string) { attempted = append(attempted, id) }

	first, err := importer.Import(t.Context(), opts)
	requirements.NoError(err)
	assertions.EqualValues(1, first.MeetingsAdded)
	state, err := loadState(st, first.SourceID)
	requirements.NoError(err)
	assertions.Equal("cursor-1", state.RelayCursor)
	assertions.False(state.RelayDone)

	source.relayDiscovery = false
	importer.now = func() time.Time { return opts.CreatedAfter.Add(10 * time.Hour) }
	source.callErr = errors.New("temporary call retrieval failure")
	_, err = importer.Import(t.Context(), opts)
	requirements.Error(err)
	state, err = loadState(st, first.SourceID)
	requirements.NoError(err)
	assertions.Equal("cursor-1", state.RelayCursor, "inactive Relay discovery must retain its pending page cursor")
	assertions.False(state.RelayDone, "inactive Relay discovery must not mark its unfinished cursor complete")

	source.callErr = nil
	source.relayDiscovery = true
	resumed, err := importer.Import(t.Context(), opts)
	requirements.NoError(err)
	assertions.EqualValues(1, resumed.MeetingsAdded)
	assertions.Equal([]string{"", "cursor-1"}, source.callPageCalls)
	assertions.Equal([]string{callA, callA, callB}, attempted)
}

func TestImporterBackfillBoundsFullStateCheckpointVolume(t *testing.T) {
	testutil.SkipIfPostgres(t, "observes checkpoint writes with a SQLite-only trigger")
	assert, require := assert.New(t), require.New(t)
	st, f, imp, opts := fixtureImporter(t)
	opts.MediaPolicy = attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone}
	_, err := st.DB().Exec(`CREATE TABLE checkpoint_observations (bytes INTEGER NOT NULL);
		CREATE TRIGGER observe_twilio_checkpoint AFTER UPDATE OF cursor_before ON sync_runs
		WHEN NEW.cursor_before IS NOT NULL
		BEGIN INSERT INTO checkpoint_observations VALUES (length(NEW.cursor_before)); END;`)
	require.NoError(err)
	baseCall := f.calls[f.recordings[0].CallSID]
	baseRecording := f.recordings[0]
	f.calls, f.recordings = map[string]Call{}, nil
	for index := range 64 {
		call, recording := baseCall, baseRecording
		call.SID = fmt.Sprintf("CA%032x", index)
		call.EndTime = "2020-01-01T10:01:00Z"
		recording.SID, recording.CallSID = fmt.Sprintf("RE%032x", index), call.SID
		f.calls[call.SID] = call
		f.recordings = append(f.recordings, recording)
	}
	summary, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(64, summary.MeetingsAdded)
	var writes, bytes int64
	require.NoError(st.DB().QueryRow("SELECT COUNT(*), COALESCE(SUM(bytes),0) FROM checkpoint_observations").Scan(&writes, &bytes))
	run, err := st.GetLastSuccessfulSync(summary.SourceID)
	require.NoError(err)
	assert.Greater(writes, int64(1), "backfill must retain intermediate progress")
	assert.LessOrEqual(writes, int64(18), "checkpoint count must be bounded independently of call count")
	assert.LessOrEqual(bytes, int64(20*len(run.CursorAfter.String)), "full-state write volume must scale linearly with final state size")
	// Keeping archival identities lets a bounded full refresh recover calls
	// omitted by provider discovery, including old terminal recordings.
	f.recordings = nil
	opts.Full, opts.Limit = true, 1
	summary, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, summary.MeetingsProcessed)
	_, count, err := st.ListMessages(0, 100)
	require.NoError(err)
	assert.EqualValues(64, count)
}

func TestImporterFullCancellationRetainsUnattemptedQueue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, opts := fixtureImporter(t)
	opts.MediaPolicy = attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone}
	baseCall := f.calls[f.recordings[0].CallSID]
	baseCall.EndTime = "2020-01-01T10:01:00Z"
	baseCall.StartTime = "2020-01-01T10:00:00Z"
	f.calls[baseCall.SID] = baseCall
	for _, letter := range []string{"b", "c"} {
		call := baseCall
		call.SID = "CA" + strings.Repeat(letter, 32)
		f.calls[call.SID] = call
		recording := f.recordings[0]
		recording.CallSID = call.SID
		recording.SID = "RE" + strings.Repeat(letter, 32)
		f.recordings = append(f.recordings, recording)
	}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	opts.Full, opts.Limit = true, 2
	ctx, cancel := context.WithCancel(t.Context())
	f.onGetCall = func(string) { cancel() }
	_, err = imp.Import(ctx, opts)
	require.Error(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, opts.Identifier)
	require.NoError(err)
	state, err := loadState(st, source.ID)
	require.NoError(err)
	assert.Equal([]string{"CA" + strings.Repeat("b", 32), "CA" + strings.Repeat("c", 32)}, state.FullQueue)
	var attempted []string
	f.onGetCall = func(id string) { attempted = append(attempted, id) }
	opts.Limit = 1
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal([]string{"CA" + strings.Repeat("b", 32)}, attempted)
}

func TestImporterFullAfterFiltersKnownCallsByCreationDate(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, source, imp, opts := fixtureImporter(t)
	opts.MediaPolicy = attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone}
	oldID, newID := "CA"+strings.Repeat("z", 32), "CA"+strings.Repeat("a", 32)
	source.calls = map[string]Call{
		oldID: {SID: oldID, Status: "completed", DateCreated: "2020-01-01T10:00:00Z", StartTime: "2020-01-01T10:00:00Z", EndTime: "2020-01-01T10:01:00Z", Duration: "60"},
		newID: {SID: newID, Status: "completed", DateCreated: "2026-10-02T10:00:00Z", StartTime: "2026-10-02T10:00:00Z", EndTime: "2026-10-02T10:01:00Z", Duration: "60"},
	}
	source.recordings = []Recording{
		{SID: "RE" + strings.Repeat("a", 32), CallSID: oldID, Status: "completed", DateCreated: "2020-01-01T10:00:00Z"},
		{SID: "RE" + strings.Repeat("b", 32), CallSID: newID, Status: "completed", DateCreated: "2026-10-02T10:00:00Z"},
	}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)

	// Leave the old call queued by a bounded full run without a cutoff. The
	// following full run must rebuild that queue for its new creation bound.
	opts.Full = true
	opts.Limit = 1
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)

	source.recordings = nil
	registeredSource, err := st.GetSourceByTypeAndIdentifier(SourceType, opts.Identifier)
	require.NoError(err)
	stateBeforeFilter, err := loadState(st, registeredSource.ID)
	require.NoError(err)
	var attempted []string
	source.onGetCall = func(id string) { attempted = append(attempted, id) }
	opts.CreatedAfter = time.Date(2026, time.October, 2, 0, 0, 0, 0, time.UTC)
	imp.now = func() time.Time { return stateBeforeFilter.Watermark.Add(time.Hour) }
	summary, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, summary.MeetingsProcessed)
	assert.Equal([]string{newID}, attempted)
	stateAfterFilter, err := loadState(st, registeredSource.ID)
	require.NoError(err)
	assert.Equal(stateBeforeFilter.Watermark, stateAfterFilter.Watermark, "a bounded full sync must preserve the incremental discovery watermark")
}

func TestImporterCreatedAfterFiltersNewCallsByCallCreationDate(t *testing.T) {
	cutoff := time.Date(2026, time.October, 2, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name           string
		callCreatedAt  string
		recordCreated  string
		relay          bool
		wantArchived   bool
		wantCallLookup bool
	}{
		{
			name:           "late-listed recording for old call",
			callCreatedAt:  "2026-10-01T23:59:59Z",
			recordCreated:  "2026-10-03T10:00:00Z",
			wantCallLookup: true,
		},
		{
			name:          "relay-discovered old call",
			callCreatedAt: "2026-10-01T23:59:59Z",
			relay:         true,
		},
		{
			name:           "unknown call creation date",
			wantCallLookup: true,
		},
		{
			name:           "call created on inclusive cutoff",
			callCreatedAt:  cutoff.Format(time.RFC3339),
			recordCreated:  cutoff.Format(time.RFC3339),
			wantArchived:   true,
			wantCallLookup: true,
		},
		{
			name:           "relay-discovered call created on cutoff",
			callCreatedAt:  cutoff.Format(time.RFC3339),
			relay:          true,
			wantArchived:   true,
			wantCallLookup: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, source, importer, opts := fixtureImporter(t)
			opts.MediaPolicy = attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone}
			opts.CreatedAfter = cutoff
			call := source.calls[testCA]
			call.DateCreated = tc.callCreatedAt
			source.calls[testCA] = call
			if tc.relay {
				source.recordings = nil
				source.listedCalls = []Call{call}
				source.relayDiscovery = true
			} else {
				source.recordings[0].DateCreated = tc.recordCreated
			}
			var attempted []string
			source.onGetCall = func(id string) { attempted = append(attempted, id) }

			summary, err := importer.Import(t.Context(), opts)
			require.NoError(err)
			messages, total, err := st.ListMessages(0, 10)
			require.NoError(err)
			wantAttempted := []string(nil)
			if tc.wantCallLookup {
				wantAttempted = []string{testCA}
			}
			assert.Equal(wantAttempted, attempted)
			if tc.wantArchived {
				assert.EqualValues(1, total)
				assert.EqualValues(1, summary.MeetingsAdded)
				assert.Len(messages, 1)
				assert.Equal(1, source.recordingListCalls)
			} else {
				assert.Zero(total)
				assert.Zero(summary.MeetingsAdded)
				assert.Empty(messages)
				assert.Zero(source.recordingListCalls)
			}
		})
	}
}

func TestImporterCreatedAfterDoesNotSuppressDueRecordingRetry(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, source, importer, opts := fixtureImporter(t)

	call := source.calls[testCA]
	call.DateCreated = "2026-10-03T09:00:00Z"
	source.calls[testCA] = call
	recordingID := source.recordings[0].SID
	source.audioErr = &APIError{Service: "recording", StatusCode: http.StatusNotFound}
	firstAttempt := importer.now()

	_, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(1, source.audioCalls)
	archivedSource, err := st.GetSourceByTypeAndIdentifier(SourceType, opts.Identifier)
	require.NoError(err)
	state, err := loadState(st, archivedSource.ID)
	require.NoError(err)
	assert.Equal(firstAttempt.Add(6*time.Hour), state.Known[testCA].NextAttempt)
	messages, total, err := st.ListMessages(0, 10)
	require.NoError(err)
	require.EqualValues(1, total)
	message, err := st.GetMessage(messages[0].ID)
	require.NoError(err)
	require.Len(message.Attachments, 1)
	assert.Empty(message.Attachments[0].ContentHash)
	var attachmentState string
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT attachment_state FROM attachments WHERE message_id = ?"), message.ID).Scan(&attachmentState))
	assert.Equal(string(attachmentpolicy.StatePending), attachmentState)
	rows, err := st.MessageMetadataBatch(archivedSource.ID, []string{testCA})
	require.NoError(err)
	raw, err := st.GetMessageRawContext(t.Context(), rows[testCA].ID)
	require.NoError(err)
	var archived struct {
		Recordings []Recording `json:"recordings"`
	}
	require.NoError(json.Unmarshal(raw, &archived))
	require.Len(archived.Recordings, 1)
	assert.Equal(recordingID, archived.Recordings[0].SID)

	// The independently due retry remains eligible even when the requested
	// history window starts after this call's creation date.
	source.audioErr = nil
	opts.CreatedAfter = time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	importer.now = func() time.Time { return firstAttempt.Add(8 * 24 * time.Hour) }

	summary, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(1, summary.MaintenanceRetries)
	assert.EqualValues(1, summary.MeetingsProcessed)
	assert.Equal(2, source.audioCalls, "the due retry should fetch the recording again")
	state, err = loadState(st, summary.SourceID)
	require.NoError(err)
	assert.True(state.Known[testCA].NextAttempt.IsZero(), "successful recovery after the retry window should clear retry state")
	assert.False(state.Known[testCA].Failed)

	messages, total, err = st.ListMessages(0, 10)
	require.NoError(err)
	require.EqualValues(1, total)
	message, err = st.GetMessage(messages[0].ID)
	require.NoError(err)
	require.Len(message.Attachments, 1)
	assert.NotEmpty(message.Attachments[0].ContentHash)
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT attachment_state FROM attachments WHERE message_id = ?"), message.ID).Scan(&attachmentState))
	assert.Equal(string(attachmentpolicy.StateStored), attachmentState)
}

func TestImporterReconcilesStoredAudioAfterOccurrenceWriteFailure(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite-only trigger to simulate an attachment occurrence write failure")
	assert, require := assert.New(t), require.New(t)
	st, source, importer, opts := fixtureImporter(t)
	recordingID := source.recordings[0].SID
	_, err := st.DB().Exec("CREATE TRIGGER fail_twilio_recording_occurrence BEFORE INSERT ON attachments " +
		"WHEN NEW.source_attachment_id = 'twilio:" + recordingID + "' " +
		"BEGIN SELECT RAISE(ABORT, 'synthetic attachment occurrence failure'); END")
	require.NoError(err)
	_, err = importer.Import(t.Context(), opts)
	require.Error(err)
	assert.Equal(1, source.audioCalls)

	archivedSource, err := st.GetSourceByTypeAndIdentifier(SourceType, opts.Identifier)
	require.NoError(err)
	rows, err := st.MessageMetadataBatch(archivedSource.ID, []string{testCA})
	require.NoError(err)
	raw, err := st.GetMessageRawContext(t.Context(), rows[testCA].ID)
	require.NoError(err)
	var evidence struct {
		RecordingArtifacts map[string]struct {
			ContentHash string `json:"content_hash"`
			StoragePath string `json:"storage_path"`
		} `json:"recording_artifacts"`
	}
	require.NoError(json.Unmarshal(raw, &evidence))
	descriptor, ok := evidence.RecordingArtifacts[recordingID]
	require.True(ok)
	require.NotEmpty(descriptor.ContentHash)
	require.NotEmpty(descriptor.StoragePath)

	_, err = st.DB().Exec("DROP TRIGGER fail_twilio_recording_occurrence")
	require.NoError(err)
	source.recordings = nil
	opts.Full = true
	_, err = importer.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(1, source.audioCalls, "a durable blob descriptor should repair the occurrence without another download")

	messages, total, err := st.ListMessages(0, 10)
	require.NoError(err)
	require.EqualValues(1, total)
	message, err := st.GetMessage(messages[0].ID)
	require.NoError(err)
	require.Len(message.Attachments, 1)
	assert.Equal(descriptor.ContentHash, message.Attachments[0].ContentHash)
	var attachmentState string
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT attachment_state FROM attachments WHERE message_id = ?"), message.ID).Scan(&attachmentState))
	assert.Equal(string(attachmentpolicy.StateStored), attachmentState)
	_, err = os.Stat(filepath.Join(opts.AttachmentsDir, descriptor.StoragePath))
	require.NoError(err)
}

func TestImporterSizeCapSkipWaitsForFullRetry(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, source, importer, opts := fixtureImporter(t)
	opts.MediaPolicy = attachmentpolicy.Policy{MaxBytes: 8}
	_, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	require.Equal(1, source.audioCalls)

	firstAttempt := importer.now()
	importer.now = func() time.Time { return firstAttempt.Add(6 * time.Hour) }
	_, err = importer.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(1, source.audioCalls, "an ordinary retry must not redownload a size-capped recording")

	opts.Full = true
	opts.MediaPolicy = attachmentpolicy.Policy{MaxBytes: 100}
	_, err = importer.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(2, source.audioCalls, "a full sync must reconsider a size-capped recording after policy changes")
	messages, _, err := st.ListMessages(0, 10)
	require.NoError(err)
	message, err := st.GetMessage(messages[0].ID)
	require.NoError(err)
	require.Len(message.Attachments, 1)
	var attachmentState string
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT attachment_state FROM attachments WHERE message_id = ?"), message.ID).Scan(&attachmentState))
	assert.Equal(string(attachmentpolicy.StateStored), attachmentState)
}

func TestImporterMediaPolicySkipsWithoutFetching(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, opts := fixtureImporter(t)
	opts.MediaPolicy = attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone}
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Zero(sum.AttachmentsStored)
	assert.Zero(f.audioCalls)
	messages, _, err := st.ListMessages(0, 10)
	require.NoError(err)
	msg, err := st.GetMessage(messages[0].ID)
	require.NoError(err)
	assert.Len(msg.Attachments, 1)
}

func TestImporterKeepsSameIDUsableTranscriptAfterPendingAndEmpty(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, opts := fixtureImporter(t)
	tr := Transcript{Kind: "classic", ID: "GT" + strings.Repeat("a", 32), SourceID: f.recordings[0].SID, Status: "completed", Complete: true, Usable: true, Segments: []Segment{{Text: "Retained planning evidence"}}}
	f.evidence.Transcripts = []Transcript{tr}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	for _, replacement := range []Transcript{{Kind: tr.Kind, ID: tr.ID, SourceID: tr.SourceID, Status: "queued"}, {Kind: tr.Kind, ID: tr.ID, SourceID: tr.SourceID, Status: "completed", Complete: true}} {
		f.evidence.Transcripts = []Transcript{replacement}
		opts.Full = true
		_, err = imp.Import(t.Context(), opts)
		require.NoError(err)
		messages, _, e := st.ListMessages(0, 10)
		require.NoError(e)
		msg, e := st.GetMessage(messages[0].ID)
		require.NoError(e)
		assert.Contains(msg.Body, "Retained planning evidence")
	}
}

func TestImporterKeepsSameIDTranscriptAfterWhitespaceRefreshFromClient(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, source, importer, opts := fixtureImporter(t)
	opts.MediaPolicy = attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone}
	transcriptionText := "Retained transcript from Twilio"
	provider := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodGet, r.Method)
		assertions.Equal("/2010-04-01/Accounts/"+testAC+"/Recordings/"+testRE+"/Transcriptions.json", r.URL.Path)
		writeJSON(t, w, map[string]any{"transcriptions": []any{map[string]any{
			"sid": "TR" + strings.Repeat("a", 32), "account_sid": testAC, "recording_sid": testRE,
			"status": "completed", "transcription_text": transcriptionText,
		}}})
	}, func(options *Options) { options.Region = "ie1" })
	var latest Evidence
	source.transcriptReader = func(ctx context.Context, call Call, recordings []Recording) (Evidence, error) {
		var err error
		latest, err = provider.Transcripts(ctx, call, recordings)
		return latest, err
	}

	_, err := importer.Import(t.Context(), opts)
	requirements.NoError(err)
	messages, _, err := st.ListMessages(0, 10)
	requirements.NoError(err)
	requirements.Len(messages, 1)
	message, err := st.GetMessage(messages[0].ID)
	requirements.NoError(err)
	assertions.Contains(message.Body, transcriptionText)

	transcriptionText = " \t\n "
	opts.Full = true
	_, err = importer.Import(t.Context(), opts)
	requirements.NoError(err)
	requirements.Len(latest.Transcripts, 1)
	assertions.False(latest.Transcripts[0].Usable)
	assertions.Empty(latest.Transcripts[0].Segments)
	messages, _, err = st.ListMessages(0, 10)
	requirements.NoError(err)
	requirements.Len(messages, 1)
	message, err = st.GetMessage(messages[0].ID)
	requirements.NoError(err)
	assertions.Contains(message.Body, "Retained transcript from Twilio")
}

func TestCanonicalBatchDeduplicatesCommunicationsAcrossRecordings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	comm := Communication{ID: "comm_example", ChannelID: "CA" + strings.Repeat("a", 32)}
	comm.Content.Type = "TEXT"
	comm.Content.Text = "Same retained speech"
	transcripts := []Transcript{
		{Kind: "batch", ID: "voice_transcription_a", SourceID: "REa", Complete: true, Usable: true, Segments: []Segment{{Text: comm.Content.Text}}, Communications: []Communication{comm}},
		{Kind: "batch", ID: "voice_transcription_b", SourceID: "REb", Complete: true, Usable: true, Segments: []Segment{{Text: comm.Content.Text}}, Communications: []Communication{comm}},
	}
	segments, complete := canonicalSegments(transcripts)
	assert.True(complete)
	require.Len(segments, 1)
	assert.Equal(comm.Content.Text, segments[0].Text)
}
func TestImporterMultipleRecordingsAndCallLegs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, opts := fixtureImporter(t)
	second := f.recordings[0]
	second.SID = "RE" + strings.Repeat("b", 32)
	child := f.recordings[0]
	child.SID = "RE" + strings.Repeat("c", 32)
	child.CallSID = "CA" + strings.Repeat("b", 32)
	call := f.calls[f.recordings[0].CallSID]
	call.ParentCallSID = call.SID
	call.SID = child.CallSID
	f.calls[call.SID] = call
	f.recordings = append(f.recordings, second, child)
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.EqualValues(2, sum.MeetingsAdded)
	assert.EqualValues(3, sum.AttachmentsStored)
	messages, total, err := st.ListMessages(0, 10)
	require.NoError(err)
	assert.EqualValues(2, total)
	counts := []int{}
	for _, m := range messages {
		msg, e := st.GetMessage(m.ID)
		require.NoError(e)
		counts = append(counts, len(msg.Attachments))
		if len(msg.Attachments) == 2 {
			assert.NotEmpty(msg.Attachments[0].ContentHash)
			assert.Equal(msg.Attachments[0].ContentHash, msg.Attachments[1].ContentHash,
				"separate recording SIDs may preserve identical bytes as source-part keyed occurrences")
		}
	}
	assert.ElementsMatch([]int{1, 2}, counts)
}

func TestImporterSizePolicyAndExpiredMissingMediaAreTerminal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fetch   error
		expired bool
	}{
		{"size", ErrMediaSizeLimit, false}, {"gone", &APIError{Service: "recording", StatusCode: 404}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, f, imp, opts := fixtureImporter(t)
			f.audioErr = tc.fetch
			if tc.expired {
				imp.now = func() time.Time { return time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC) }
			}
			sum, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			assert.Zero(sum.AttachmentsStored)
			if tc.name == "size" {
				messages, total, listErr := st.ListMessages(0, 10)
				require.NoError(listErr)
				require.EqualValues(1, total)
				msg, getErr := st.GetMessage(messages[0].ID)
				require.NoError(getErr)
				require.Len(msg.Attachments, 1)
				assert.Zero(msg.Attachments[0].Size)
			}
			source, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
			require.NoError(err)
			state, err := loadState(st, source.ID)
			require.NoError(err)
			assert.False(state.Known[f.recordings[0].CallSID].Failed)
		})
	}
}
