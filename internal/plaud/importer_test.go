package plaud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type fakeSource struct {
	pages      [][]File
	recordings map[string]Recording
	fail       map[string]error
	read       []string
	email      string
	lastPage   int
	onRead     func(string)
}

func (f *fakeSource) CurrentUser(context.Context) (string, error) {
	if f.email != "" {
		return f.email, nil
	}
	return "user@example.com", nil
}
func (f *fakeSource) ListFiles(_ context.Context, page, _ int) (FilePage, error) {
	f.lastPage = page
	if page > len(f.pages) {
		return FilePage{Files: []File{}}, nil
	}
	return FilePage{Files: f.pages[page-1]}, nil
}
func (f *fakeSource) Recording(_ context.Context, id string) (Recording, error) {
	f.read = append(f.read, id)
	if f.onRead != nil {
		f.onRead(id)
	}
	if err := f.fail[id]; err != nil {
		return Recording{}, err
	}
	return f.recordings[id], nil
}

func fixture(id string) Recording {
	return Recording{File: File{ID: id, Name: "Planning", StartedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), DurationMS: 90000}, Notes: []Note{{ID: "summary", Type: "auto_sum_note", Content: "Release summary"}}, Segments: []Segment{{Speaker: "Speaker 1", Text: "Ship the searchable release", StartSeconds: 2}}, TranscriptBlock: "transaction_polish"}
}

func sourceFixture(ids ...string) *fakeSource {
	f := &fakeSource{recordings: map[string]Recording{}, fail: map[string]error{}}
	var page []File
	for _, id := range ids {
		r := fixture(id)
		f.recordings[id] = r
		page = append(page, r.File)
	}
	f.pages = [][]File{page}
	return f
}

func registered(t *testing.T) (*store.Store, *store.Source) {
	t.Helper()
	st := testutil.NewTestStore(t)
	src, err := RegisterSource(st, "personal", "user@example.com")
	require.NoError(t, err)
	return st, src
}

func TestRegisterSourceSetsDisplayNameAndPreservesOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := RegisterSource(st, " work ", "user@example.com")
	require.NoError(err)
	assert.Equal("work", src.DisplayName.String)

	_, err = RegisterSource(st, "work", "other@example.com")
	require.Error(err)
	src, err = st.GetSourceByTypeAndIdentifier(SourceType, "work")
	require.NoError(err)
	assert.Equal("work", src.DisplayName.String)
	assert.NoError(ValidateOwner(src, "user@example.com"))
}

func TestImporterReconcilesEditsAndPreservesMissingEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, src := registered(t)
	f := sourceFixture("f")
	imp := NewImporter(st, f)
	opts := ImportOptions{Identifier: "personal", AccountEmail: "user@example.com"}
	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsAdded)
	ids, err := st.MessageExistsBatch(src.ID, []string{"f"})
	require.NoError(err)
	mid := ids["f"]
	require.NotZero(mid)
	sum, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Zero(sum.MeetingsUpdated)
	r := f.recordings["f"]
	r.Segments[0].Speaker = "Alex Example"
	r.Notes[0].ID = "regenerated-summary"
	r.Notes[0].Content = "Updated summary"
	f.recordings["f"] = r
	sum, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsUpdated)
	ids, err = st.MessageExistsBatch(src.ID, []string{"f"})
	require.NoError(err)
	assert.Equal(mid, ids["f"])
	r.Segments = nil
	r.Notes = []Note{{ID: "regenerated-summary", Type: "auto_sum_note", Content: ""}}
	f.recordings["f"] = r
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	raw, err := st.GetMessageRaw(mid)
	require.NoError(err)
	c := meetingcontent.Decode("plaud_json", raw, nil)
	assert.NotContains(string(raw), `"transcript":`)
	assert.Equal(meetingcontent.StateAvailable, c.Transcript.State)
	require.Len(c.Transcript.Segments, 1)
	assert.Equal("Alex Example", c.Transcript.Segments[0].Speaker)
	assert.Equal("Updated summary", c.Summary.Text)
	body, err := st.GetMessageBodyText(mid)
	require.NoError(err)
	assert.Contains(body, "Ship the searchable release")
	assert.NotContains(body, "Release summary")
	_, hits, err := st.SearchMessages("searchable", 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), hits)
	assert.NotContains(string(raw), "presigned")
	assert.NotContains(string(raw), "data_link")
	f.pages = nil
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	ids, err = st.MessageExistsBatch(src.ID, []string{"f"})
	require.NoError(err)
	assert.Equal(mid, ids["f"])
}

func TestImporterEnumeratesShortPagesBeyondFiveHundred(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _ := registered(t)
	f := sourceFixture()
	f.pages = nil
	for i := range 503 {
		id := fmt.Sprintf("f-%03d", i)
		r := fixture(id)
		f.recordings[id] = r
		f.pages = append(f.pages, []File{r.File})
	}
	sum, err := NewImporter(st, f).Import(context.Background(), ImportOptions{Identifier: "personal", AccountEmail: "user@example.com", Limit: 1})
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsAdded)
	// Enumeration cannot stop at a short page or the filtered-search cap.
	assert.Equal("f-000", f.read[0])
	assert.Equal(504, f.lastPage)
}

func TestLimitedDateScopedRunsRotate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, src := registered(t)
	f := sourceFixture("c", "a", "b")
	imp := NewImporter(st, f)
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	opts := ImportOptions{Identifier: "personal", AccountEmail: "user@example.com", Limit: 1, CreatedAfter: &after}
	for range 4 {
		_, err := imp.Import(context.Background(), opts)
		require.NoError(err)
	}
	assert.Equal([]string{"a", "b", "c", "a"}, f.read)
	last, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	var state syncState
	require.NoError(json.Unmarshal([]byte(last.CursorAfter.String), &state))
	require.Len(state.LastChecked, 3)
}

func TestLimitedRunsStartWithNewestUnseenRecordings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _ := registered(t)
	f := sourceFixture("a-old", "m-created", "z-new")

	old := f.recordings["a-old"]
	old.File.StartedAt = time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	f.recordings[old.File.ID] = old
	created := f.recordings["m-created"]
	created.File.StartedAt = time.Time{}
	created.File.CreatedAt = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f.recordings[created.File.ID] = created
	newest := f.recordings["z-new"]
	newest.File.StartedAt = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	f.recordings[newest.File.ID] = newest
	f.pages[0] = []File{old.File, created.File, newest.File}

	imp := NewImporter(st, f)
	for range 3 {
		_, err := imp.Import(context.Background(), ImportOptions{Identifier: "personal", AccountEmail: "user@example.com", Limit: 1})
		require.NoError(err)
	}

	assert.Equal([]string{"z-new", "m-created", "a-old"}, f.read)
}

func TestLimitedRunsRotatePastFailedRecordings(t *testing.T) {
	for _, tc := range []struct {
		name       string
		limit      int
		wantErrors []bool
	}{
		{name: "one recording", limit: 1, wantErrors: []bool{true, false, false, true}},
		{name: "mixed batch", limit: 2, wantErrors: []bool{true, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, src := registered(t)
			f := sourceFixture("a", "b", "c")
			f.fail["a"] = ErrContract
			opts := ImportOptions{Identifier: "personal", AccountEmail: "user@example.com", Limit: tc.limit}
			for index, wantError := range tc.wantErrors {
				// Recreate the importer so rotation must come from the archive.
				imp := NewImporter(st, f)
				imp.now = func() time.Time { return time.Date(2026, 9, 1, index, 0, 0, 0, time.UTC) }
				sum, err := imp.Import(t.Context(), opts)
				if wantError {
					require.ErrorIs(err, ErrContract)
					assert.Equal(int64(1), sum.Errors)
				} else {
					require.NoError(err)
				}
				latest, err := st.GetLatestSync(src.ID)
				require.NoError(err)
				if wantError {
					assert.Equal("failed", latest.Status)
				} else {
					assert.Equal("completed", latest.Status)
				}
			}
			assert.Equal([]string{"a", "b", "c", "a"}, f.read)
			ids, err := st.MessageExistsBatch(src.ID, []string{"a", "b", "c"})
			require.NoError(err)
			assert.Zero(ids["a"])
			assert.NotZero(ids["b"])
			assert.NotZero(ids["c"])
		})
	}
}

func TestFailureKeepsLastSuccessfulCursorAndPartialWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, src := registered(t)
	f := sourceFixture("a", "b")
	imp := NewImporter(st, f)
	opts := ImportOptions{Identifier: "personal", AccountEmail: "user@example.com"}
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	before, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	r := f.recordings["a"]
	r.Notes[0].Content = "A landed change"
	f.recordings["a"] = r
	f.fail["b"] = errors.New("upstream failed")
	sum, err := imp.Import(context.Background(), opts)
	require.Error(err)
	assert.Equal(int64(1), sum.MeetingsUpdated)
	assert.Equal(int64(1), sum.Errors)
	after, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	assert.Equal(before.ID, after.ID)
	assert.Equal(before.CursorAfter, after.CursorAfter)
}

func TestOwnerAndMissingSourceRefusedBeforeContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, src := registered(t)
	f := sourceFixture("f")
	_, err := RegisterSource(st, "personal", "other@example.com")
	require.Error(err)
	_, err = NewImporter(st, f).Import(context.Background(), ImportOptions{Identifier: "personal", AccountEmail: "other@example.com"})
	require.Error(err)
	assert.Empty(f.read)
	f.email = "other@example.com"
	_, err = NewImporter(st, f).Import(context.Background(), ImportOptions{Identifier: "personal", AccountEmail: "user@example.com"})
	require.Error(err)
	assert.Empty(f.read)
	require.NoError(st.RemoveSource(src.ID))
	_, err = NewImporter(st, f).Import(context.Background(), ImportOptions{Identifier: "personal", AccountEmail: "user@example.com"})
	require.Error(err)
	assert.Contains(err.Error(), "add-plaud")
}

func TestRepeatedListPageAndCancellationFailRun(t *testing.T) {
	st, _ := registered(t)
	f := sourceFixture("f")
	f.pages = append(f.pages, f.pages[0])
	opts := ImportOptions{Identifier: "personal", AccountEmail: "user@example.com"}
	_, err := NewImporter(st, f).Import(context.Background(), opts)
	require.Error(t, err)
	assert.Empty(t, f.read)
	f = sourceFixture("a", "b")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onRead = func(string) { cancel() }
	_, err = NewImporter(st, f).Import(ctx, opts)
	require.ErrorIs(t, err, context.Canceled)
}
