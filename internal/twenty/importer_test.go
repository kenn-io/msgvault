package twenty

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type fakeSource struct {
	pages         map[string]*Page
	calendars     map[string]*Calendar
	calendarError error
	listHook      func(string)
}

func (f *fakeSource) Probe(context.Context) error { return nil }
func (f *fakeSource) ListRecordings(ctx context.Context, cursor string, _ int) (*Page, error) {
	if f.listHook != nil {
		f.listHook(cursor)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.pages[cursor], nil
}
func (f *fakeSource) GetCalendar(ctx context.Context, id string) (*Calendar, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.calendarError != nil {
		return nil, f.calendarError
	}
	return f.calendars[id], nil
}
func recordingFixture(t *testing.T, id, transcript string) Recording {
	t.Helper()
	require := require.New(t)
	raw := jsontext.Value(fmt.Sprintf(`{"id":%q,"title":"Planning","status":"COMPLETED","createdAt":"2026-09-01T10:00:00Z","startedAt":"2026-09-01T10:00:00Z","endedAt":"2026-09-01T10:10:00Z","calendarEventId":"event-1","summary":{"markdown":"Launch decision"},"transcript":%s}`, id, transcript))
	var r Recording
	require.NoError(json.Unmarshal(raw, &r))
	r.Raw = raw
	return r
}
func calendarFixture() *Calendar {
	return &Calendar{Raw: jsontext.Value(`{"id":"event-1","startsAt":"2026-09-01T09:30:00Z","endsAt":"2026-09-01T10:30:00Z"}`), Participants: []Participant{{Raw: jsontext.Value(`{"id":"p1","handle":"recorder@example.com","displayName":"Recorder Example","isOrganizer":true}`)}, {Raw: jsontext.Value(`{"id":"p2","handle":"attendee@example.com","displayName":"Attendee Example","isOrganizer":false}`)}, {Raw: jsontext.Value(`{"id":"p3","handle":"display only","displayName":"Guest"}`)}}}
}
func newImportFixture(t *testing.T) (*store.Store, *fakeSource, ImportOptions) {
	t.Helper()
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	f := &fakeSource{pages: map[string]*Page{"": {Records: []Recording{recordingFixture(t, "r1", `[{"participant":{"name":"Speaker Example"},"words":[{"text":"Searchableword","start_timestamp":{"relative":2}}]}]`)}}}, calendars: map[string]*Calendar{"event-1": calendarFixture()}}
	return st, f, ImportOptions{Identifier: "work", AccountEmail: "recorder@example.com"}
}
func archivedID(t *testing.T, st *store.Store, id string) int64 {
	t.Helper()
	require := require.New(t)
	var result int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_message_id=?`), "recording:"+id).Scan(&result))
	return result
}
func TestImportTwentyArchivesAndConverges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, f, opts := newImportFixture(t)
	imp := NewImporter(st, f)
	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsAdded)
	id := archivedID(t, st, "r1")
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Contains(body, "Searchableword")
	assert.Contains(body, "Guest")
	assert.NotContains(body, "attendee@example.com")
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	content := meetingcontent.Decode(RawFormat, raw, nil)
	assert.Equal(meetingcontent.DurationProvider, content.DurationBasis)
	assert.Equal(meetingcontent.CoverageUnsupported, content.ActionCoverage)
	var kind string
	var fromMe bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT message_type,is_from_me FROM messages WHERE id=?`), id).Scan(&kind, &fromMe))
	assert.Equal("meeting_transcript", kind)
	assert.True(fromMe)
	hits, total, err := st.SearchMessages("Searchableword", 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	require.Len(hits, 1)
	sum, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Zero(sum.MeetingsUpdated)
	f.calendars["event-1"].Participants[1].Raw = jsontext.Value(`{"id":"p2","handle":"updated@example.com","displayName":"Updated Example"}`)
	sum, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsUpdated)
	assert.Equal(id, archivedID(t, st, "r1"))
	var recipients int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM message_recipients mr JOIN participants p ON p.id=mr.participant_id WHERE mr.message_id=? AND p.email_address=?`), id, "updated@example.com").Scan(&recipients))
	assert.Equal(1, recipients)
	opts.Full = true
	sum, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsUpdated)
	f.pages[""].Records = nil
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(id, archivedID(t, st, "r1"))
}
func TestImportTwentyRevisitsPendingAndLateEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, f, opts := newImportFixture(t)
	imp := NewImporter(st, f)
	r := recordingFixture(t, "r1", `{"status":"PENDING"}`)
	r.Raw = jsontext.Value(`{"id":"r1","createdAt":"2026-09-01T10:00:00Z","transcript":{"status":"PENDING"}}`)
	r.CalendarEventID = ""
	f.pages[""].Records = []Recording{r}
	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.SkippedEmpty)
	f.pages[""].Records = []Recording{recordingFixture(t, "r1", `[{"words":[{"text":"Late transcript"}]}]`)}
	sum, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsAdded)
	f.pages[""].Records = []Recording{recordingFixture(t, "r1", `[{"words":[{"text":"Revised transcript"}]}]`)}
	sum, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsUpdated)
}
func TestImportTwentyPaginationFiltersAndLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, f, opts := newImportFixture(t)
	first := &Page{HasMore: true, NextCursor: "next"}
	for i := range 100 {
		first.Records = append(first.Records, recordingFixture(t, fmt.Sprintf("r%03d", i), `[]`))
	}
	f.pages[""] = first
	f.pages["next"] = &Page{Records: []Recording{recordingFixture(t, "r100", `[]`)}}
	sum, err := NewImporter(st, f).Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(101), sum.MeetingsAdded)
	assert.NotZero(archivedID(t, st, "r100"))
	assert.False(sum.PartialCoverage)
	opts.Limit = 1
	sum, err = NewImporter(st, f).Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsProcessed)
	assert.True(sum.PartialCoverage)
	opts.Limit = 0
	opts.StartedAfter = time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	sum, err = NewImporter(st, f).Import(context.Background(), opts)
	require.NoError(err)
	assert.Zero(sum.MeetingsProcessed)
}
func TestImportTwentyFailsWithoutOverwritingEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, f, opts := newImportFixture(t)
	imp := NewImporter(st, f)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	id := archivedID(t, st, "r1")
	before, err := st.GetMessageRaw(id)
	require.NoError(err)
	f.calendarError = errors.New("calendar unavailable")
	_, err = imp.Import(context.Background(), opts)
	require.Error(err)
	after, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(before, after)
	run, err := st.GetLatestSync(1)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, run.Status)
	require.NoError(st.RemoveSource(1))
	_, err = imp.Import(context.Background(), opts)
	require.Error(err)
	_, err = st.GetSourceByTypeAndIdentifier(SourceType, "work")
	require.ErrorIs(err, store.ErrSourceNotFound)
}

func TestImportTwentyFailsWhenArchiveEnvelopeExceedsContentLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, f, opts := newImportFixture(t)
	imp := NewImporter(st, f)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	id := archivedID(t, st, "r1")
	before, err := st.GetMessageRaw(id)
	require.NoError(err)

	const contentLimit = 64 << 20
	const prefix = `{"id":"r1","createdAt":"2026-09-01T10:00:00Z","summary":{"markdown":"`
	const suffix = `"}}`
	recording := recordingFixture(t, "r1", `[]`)
	recording.CalendarEventID = ""
	recording.Raw = jsontext.Value(prefix + strings.Repeat("x", contentLimit-1-len(prefix)-len(suffix)) + suffix)
	require.LessOrEqual(len(recording.Raw), contentLimit-1)
	f.pages[""].Records = []Recording{recording}

	_, err = imp.Import(t.Context(), opts)
	require.ErrorContains(err, "twenty archive evidence exceeds the meeting content size limit")
	after, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(before, after)
	run, err := st.GetLatestSync(1)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, run.Status)
}

func TestImportTwentyFailedSecondPageRetainsCommittedCounts(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancel), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, f, opts := newImportFixture(t)
			f.pages[""].HasMore = true
			f.pages[""].NextCursor = "next"
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			if cancel {
				f.listHook = func(cursor string) {
					if cursor == "next" {
						stop()
					}
				}
			}
			sum, err := NewImporter(st, f).Import(ctx, opts)
			require.Error(err)
			assert.Equal(int64(1), sum.MeetingsAdded)
			run, err := st.GetLatestSync(sum.SourceID)
			require.NoError(err)
			assert.Equal(store.SyncStatusFailed, run.Status)
			assert.Equal(int64(1), run.MessagesAdded)
			assert.NotZero(archivedID(t, st, "r1"))
		})
	}
}
func TestImportTwentyRejectsLoopAndInvalidOptions(t *testing.T) {
	require := require.New(t)
	st, f, opts := newImportFixture(t)
	opts.Limit = -1
	_, err := NewImporter(st, f).Import(context.Background(), opts)
	require.Error(err)
	opts.Limit = 0
	f.pages[""].HasMore = true
	f.pages[""].NextCursor = "next"
	f.pages["next"] = f.pages[""]
	_, err = NewImporter(st, f).Import(context.Background(), opts)
	require.Error(err)
}

func TestImportTwentyHTTPPartialErrorPreservesArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, f, opts := newImportFixture(t)
	_, err := NewImporter(st, f).Import(t.Context(), opts)
	require.NoError(err)
	id := archivedID(t, st, "r1")
	before, err := st.GetMessageRaw(id)
	require.NoError(err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":{"callRecordings":{"edges":[],"pageInfo":{"hasNextPage":false}}},"errors":[{"message":"permission denied"}]}`)
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "example-key")
	require.NoError(err)
	_, err = NewImporter(st, client).Import(t.Context(), opts)
	require.Error(err)
	after, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(before, after)
}

func TestImportTwentyArchivesRecordingWhenLinkedCalendarEventIsGone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Query string `json:"query"`
		}
		if !assert.NoError(json.UnmarshalRead(r.Body, &request)) {
			return
		}
		switch {
		case strings.Contains(request.Query, "callRecordings("):
			_, _ = fmt.Fprint(w, `{"data":{"callRecordings":{"edges":[{"node":{"id":"recording-1","title":"Recording fallback","status":"COMPLETED","createdAt":"2026-09-01T10:00:00Z","startedAt":"2026-09-01T10:00:00Z","endedAt":"2026-09-01T10:10:00Z","calendarEventId":"deleted-event","summary":{"markdown":"Kept summary"},"transcript":null}}],"pageInfo":{"hasNextPage":false}}}}`)
		case strings.Contains(request.Query, "calendarEvents("):
			_, _ = fmt.Fprint(w, `{"data":{"calendarEvents":{"edges":[]}}}`)
		default:
			assert.Fail("unexpected Twenty query")
		}
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "missing-calendar-import-example-key")
	require.NoError(err)

	summary, err := NewImporter(st, client).Import(t.Context(), ImportOptions{Identifier: "work", AccountEmail: "recorder@example.com"})
	require.NoError(err)
	assert.Equal(int64(1), summary.MeetingsAdded)
	assert.Equal(2, requests, "a deleted event should not trigger a participant request")
	id := archivedID(t, st, "recording-1")
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Contains(body, "Recording fallback")
	assert.Contains(body, "Kept summary")
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Contains(string(raw), `"calendar_event":null`)
}

func TestImportTwentyOccurrenceFallbackAndMissingTime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, f, opts := newImportFixture(t)
	r := recordingFixture(t, "r1", `[]`)
	r.StartedAt = "invalid"
	f.pages[""].Records = []Recording{r}
	_, err := NewImporter(st, f).Import(t.Context(), opts)
	require.NoError(err)
	var sent time.Time
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT sent_at FROM messages WHERE id=?`), archivedID(t, st, "r1")).Scan(&sent))
	assert.Equal(time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC), sent.UTC())
	r.ID = "r2"
	r.CalendarEventID = ""
	r.CreatedAt = "invalid"
	f.pages[""].Records = []Recording{r}
	_, err = NewImporter(st, f).Import(t.Context(), opts)
	require.ErrorContains(err, "occurrence time")
}

func TestTwentyArchiveBoundsUnicodeSnippet(t *testing.T) {
	for _, transcriptOnly := range []bool{false, true} {
		t.Run(strconv.FormatBool(transcriptOnly), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			text := strings.Repeat("🙂", 250)
			recording := recordingFixture(t, "long-preview", `[]`)
			fields := map[string]any{"id": "long-preview", "createdAt": "2026-09-01T10:00:00Z"}
			if transcriptOnly {
				fields["transcript"] = []any{map[string]any{"words": []any{map[string]any{"text": text}}}}
			} else {
				fields["summary"] = map[string]any{"markdown": text}
			}
			raw, err := json.Marshal(fields)
			require.NoError(err)
			recording.Raw = raw
			snapshot, eligible, err := archiveSnapshot(1, "recorder@example.com", recording, nil)
			require.NoError(err)
			require.True(eligible)
			assert.Equal(strings.Repeat("🙂", 200), snapshot.Snippet)
			assert.Contains(snapshot.Body, text)
		})
	}
}
