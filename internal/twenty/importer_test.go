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
	pages     map[string]*Page
	listError error
	since     []string
	listHook  func(string)
}

func (f *fakeSource) Probe(context.Context) error { return nil }

// ListRecordings filters by updatedAt the way Twenty does, so incremental
// runs only see recordings changed since the watermark.
func (f *fakeSource) ListRecordings(ctx context.Context, since, cursor string, _ int) (*Page, error) {
	f.since = append(f.since, since)
	if f.listHook != nil {
		f.listHook(cursor)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.listError != nil {
		return nil, f.listError
	}
	page := f.pages[cursor]
	if page == nil {
		return nil, errors.New("no recording page")
	}
	bound, err := time.Parse(time.RFC3339Nano, since)
	if err != nil {
		return nil, fmt.Errorf("parse since: %w", err)
	}
	filtered := &Page{HasMore: page.HasMore, NextCursor: page.NextCursor}
	for _, recording := range page.Records {
		if updated, err := time.Parse(time.RFC3339Nano, recording.UpdatedAt); err != nil || !updated.Before(bound) {
			filtered.Records = append(filtered.Records, recording)
		}
	}
	return filtered, nil
}
func recordingFixture(t *testing.T, id, transcript string) Recording {
	t.Helper()
	require := require.New(t)
	raw := jsontext.Value(fmt.Sprintf(`{"id":%q,"title":"Planning","status":"COMPLETED","createdAt":"2026-09-01T10:00:00Z","updatedAt":"2026-09-01T11:00:00Z","startedAt":"2026-09-01T10:00:00Z","endedAt":"2026-09-01T10:10:00Z","calendarEventId":"event-1","summary":{"markdown":"Launch decision"},"transcript":%s}`, id, transcript))
	var r Recording
	require.NoError(json.Unmarshal(raw, &r))
	r.Raw = raw
	r.Calendar = calendarFixture()
	return r
}

// updated returns the recording as Twenty would after an edit at the given time.
func updated(t *testing.T, r Recording, at string) Recording {
	t.Helper()
	var fields map[string]any
	require.NoError(t, json.Unmarshal(r.Raw, &fields))
	fields["updatedAt"] = at
	raw, err := json.Marshal(fields, json.Deterministic(true))
	require.NoError(t, err)
	r.Raw, r.UpdatedAt = raw, at
	return r
}
func calendarFixture() *Calendar {
	return &Calendar{Raw: jsontext.Value(`{"id":"event-1","startsAt":"2026-09-01T09:30:00Z","endsAt":"2026-09-01T10:30:00Z"}`), Participants: []jsontext.Value{jsontext.Value(`{"id":"p1","handle":"recorder@example.com","displayName":"Recorder Example","isOrganizer":true}`), jsontext.Value(`{"id":"p2","handle":"attendee@example.com","displayName":"Attendee Example","isOrganizer":false}`), jsontext.Value(`{"id":"p3","handle":"display only","displayName":"Guest"}`)}}
}
func newImportFixture(t *testing.T) (*store.Store, *fakeSource, ImportOptions) {
	t.Helper()
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	f := &fakeSource{pages: map[string]*Page{"": {Records: []Recording{recordingFixture(t, "r1", `[{"participant":{"name":"Speaker Example"},"words":[{"text":"Searchableword","start_timestamp":{"relative":2}}]}]`)}}}}
	return st, f, ImportOptions{Identifier: "work", AccountEmail: "recorder@example.com"}
}
func attendeeCount(t *testing.T, st *store.Store, id int64, email string) int {
	t.Helper()
	var recipients int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM message_recipients mr JOIN participants p ON p.id=mr.participant_id WHERE mr.message_id=? AND p.email_address=?`), id, email).Scan(&recipients))
	return recipients
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
	assert.Equal([]string{"1970-01-01T00:00:00Z", "2026-09-01T11:00:00Z"}, f.since, "the second run lists only recordings updated since the first")
	f.pages[""].Records[0].Calendar.Participants[1] = jsontext.Value(`{"id":"p2","handle":"updated@example.com","displayName":"Updated Example"}`)
	opts.Full = true
	sum, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal("1970-01-01T00:00:00Z", f.since[len(f.since)-1], "full rescans every recording")
	assert.Equal(int64(1), sum.MeetingsUpdated)
	assert.Equal(id, archivedID(t, st, "r1"))
	assert.Equal(1, attendeeCount(t, st, id, "updated@example.com"))
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
	r.Raw = jsontext.Value(`{"id":"r1","createdAt":"2026-09-01T10:00:00Z","updatedAt":"2026-09-01T11:00:00Z","transcript":{"status":"PENDING"}}`)
	r.CalendarEventID = ""
	r.Calendar = nil
	f.pages[""].Records = []Recording{r}
	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.SkippedEmpty)
	f.pages[""].Records = []Recording{updated(t, recordingFixture(t, "r1", `[{"words":[{"text":"Late transcript"}]}]`), "2026-09-01T12:00:00Z")}
	sum, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsAdded)
	f.pages[""].Records = []Recording{updated(t, recordingFixture(t, "r1", `[{"words":[{"text":"Revised transcript"}]}]`), "2026-09-01T13:00:00Z")}
	sum, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsUpdated)
}

func TestImportTwentyLimitedRunResumesFromWatermark(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, f, opts := newImportFixture(t)
	f.pages[""].Records = []Recording{recordingFixture(t, "r1", `[]`), updated(t, recordingFixture(t, "r2", `[]`), "2026-09-01T12:00:00Z")}
	opts.Limit = 1
	sum, err := NewImporter(st, f).Import(t.Context(), opts)
	require.NoError(err)
	assert.True(sum.PartialCoverage)
	opts.Limit = 0
	sum, err = NewImporter(st, f).Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal("2026-09-01T11:00:00Z", f.since[1])
	assert.Equal(int64(1), sum.MeetingsAdded)
	assert.NotZero(archivedID(t, st, "r2"))
	// A run filtered by --after leaves the watermark where it was.
	opts.StartedAfter = time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	_, err = NewImporter(st, f).Import(t.Context(), opts)
	require.NoError(err)
	opts.StartedAfter = time.Time{}
	_, err = NewImporter(st, f).Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal("2026-09-01T12:00:00Z", f.since[3])
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
	opts.Limit, opts.Full = 1, true
	sum, err = NewImporter(st, f).Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsProcessed)
	assert.True(sum.PartialCoverage)
	opts.Limit, opts.Full = 0, false
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
	f.listError = errors.New("list unavailable")
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

// Repeated limited runs must move past recordings that share an updatedAt.
func TestImportTwentyRepeatedLimitedRunsPassTimestampTies(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, f, opts := newImportFixture(t)
	f.pages[""].Records = []Recording{recordingFixture(t, "r1", `[]`), recordingFixture(t, "r2", `[]`), recordingFixture(t, "r3", `[]`)}
	opts.Limit = 1
	for _, id := range []string{"r1", "r2", "r3"} {
		sum, err := NewImporter(st, f).Import(t.Context(), opts)
		require.NoError(err)
		assert.Equal(int64(1), sum.MeetingsAdded, id)
		assert.NotZero(archivedID(t, st, id))
	}
	sum, err := NewImporter(st, f).Import(t.Context(), opts)
	require.NoError(err)
	assert.Zero(sum.MeetingsProcessed)
	f.pages[""].Records = append(f.pages[""].Records, updated(t, recordingFixture(t, "r4", `[]`), "2026-09-01T12:00:00Z"))
	sum, err = NewImporter(st, f).Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsAdded)
	assert.NotZero(archivedID(t, st, "r4"))
}

func TestImportTwentySkipsRecordingOverContentLimit(t *testing.T) {
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
	recording := updated(t, recordingFixture(t, "r1", `[]`), "2026-09-01T12:00:00Z")
	recording.CalendarEventID = ""
	recording.Calendar = nil
	recording.Raw = jsontext.Value(prefix + strings.Repeat("x", contentLimit-1-len(prefix)-len(suffix)) + suffix)
	require.LessOrEqual(len(recording.Raw), contentLimit-1)
	next := updated(t, recordingFixture(t, "r2", `[]`), "2026-09-01T12:00:00Z")
	f.pages[""].Records = []Recording{recording, next}

	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.SkippedInvalid)
	assert.Equal(int64(1), sum.MeetingsAdded, "the recording after the bad one is still archived")
	after, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(before, after)
	run, err := st.GetLatestSync(sum.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, run.Status)
	items, err := st.ListSyncRunItems(run.ID, store.SyncRunItemStatusSkipped, 10)
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal("recording:r1", items[0].SourceMessageID)
	assert.Contains(items[0].ErrorMessage, "content size limit")
}

func TestImportTwentySkipsRecordingTooLargeToList(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, f, opts := newImportFixture(t)
	f.pages[""].Records = append([]Recording{{ID: "huge", UpdatedAt: "2026-09-01T11:00:00Z", TooLarge: true}}, f.pages[""].Records...)
	sum, err := NewImporter(st, f).Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.SkippedInvalid)
	assert.Equal(int64(1), sum.MeetingsAdded)
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
func TestImportTwentyRejectsInvalidOptions(t *testing.T) {
	require := require.New(t)
	st, f, opts := newImportFixture(t)
	opts.Limit = -1
	_, err := NewImporter(st, f).Import(context.Background(), opts)
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
		assert.Contains(request.Query, "callRecordings(")
		_, _ = fmt.Fprint(w, `{"data":{"callRecordings":{"edges":[{"node":{"id":"recording-1","title":"Recording fallback","status":"COMPLETED","createdAt":"2026-09-01T10:00:00Z","updatedAt":"2026-09-01T11:00:00Z","startedAt":"2026-09-01T10:00:00Z","endedAt":"2026-09-01T10:10:00Z","calendarEventId":"deleted-event","calendarEvent":null,"summary":{"markdown":"Kept summary"},"transcript":null}}],"pageInfo":{"hasNextPage":false}}}}`)
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "missing-calendar-import-example-key")
	require.NoError(err)

	summary, err := NewImporter(st, client).Import(t.Context(), ImportOptions{Identifier: "work", AccountEmail: "recorder@example.com"})
	require.NoError(err)
	assert.Equal(int64(1), summary.MeetingsAdded)
	assert.Equal(1, requests, "a deleted event should not trigger a participant request")
	id := archivedID(t, st, "recording-1")
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Contains(body, "Recording fallback")
	assert.Contains(body, "Kept summary")
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Contains(string(raw), `"calendar_event":null`)
}

func TestImportTwentyKeepsAttendeesWhenCalendarEventDisappears(t *testing.T) {
	for _, unlinked := range []bool{false, true} {
		t.Run(strconv.FormatBool(unlinked), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, f, opts := newImportFixture(t)
			imp := NewImporter(st, f)
			_, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			id := archivedID(t, st, "r1")
			require.Equal(1, attendeeCount(t, st, id, "attendee@example.com"))

			later := updated(t, recordingFixture(t, "r1", `[{"words":[{"text":"Late transcript"}]}]`), "2026-09-01T12:00:00Z")
			later.Calendar = nil
			if unlinked {
				later.CalendarEventID = ""
			}
			f.pages[""].Records = []Recording{later}
			for _, full := range []bool{false, true} {
				opts.Full = full
				_, err = imp.Import(t.Context(), opts)
				require.NoError(err)
				assert.Equal(1, attendeeCount(t, st, id, "attendee@example.com"))
				raw, err := st.GetMessageRaw(id)
				require.NoError(err)
				assert.Contains(string(raw), "Late transcript")
				assert.Contains(string(raw), `"startsAt":"2026-09-01T09:30:00Z"`)
			}
		})
	}
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
	r.Calendar = nil
	r.CreatedAt = "invalid"
	f.pages[""].Records = []Recording{r, recordingFixture(t, "r3", `[]`)}
	sum, err := NewImporter(st, f).Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.SkippedInvalid)
	assert.NotZero(archivedID(t, st, "r3"))
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
