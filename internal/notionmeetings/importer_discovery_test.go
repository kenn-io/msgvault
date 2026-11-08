package notionmeetings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func broadFixture(t *testing.T) (*store.Store, *discoveryFixture, *Importer) {
	t.Helper()
	st, fake, imp := newImporterFixture(t)
	source := &discoveryFixture{fakeImportSource: fake}
	imp.client = source
	return st, source, imp
}
func TestImporterMatchesRefreshedHeaderBeforeSections(t *testing.T) {
	for _, tc := range []struct {
		name, creator string
		attendees     []string
		want          int64
	}{{"creator", "creator-1", nil, 1}, {"attendee", "other", []string{"creator-1"}, 1}, {"both", "creator-1", []string{"creator-1"}, 1}, {"editor only", "other", nil, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st, source, imp := broadFixture(t)
			meeting := hydrationMeeting()
			meeting.CreatedBy.ID = tc.creator
			meeting.LastEditedBy.ID = "creator-1"
			meeting.MeetingNotes.CalendarEvent.Attendees = tc.attendees
			installMeetingHeader(t, source.fakeHydrationSource, meeting)
			if tc.want == 0 {
				source.blockErrs = map[string]error{"summary-1": ErrUnauthorized, "transcript-1": ErrUnauthorized}
			}
			sum, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
			require.NoError(err)
			assert.Equal(tc.want, sum.MeetingsAdded)
			var n int
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&n))
			assert.Equal(int(tc.want), n)
		})
	}
}
func TestImporterOldReadyRefreshIndependentOfAfterAndLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, source, imp := broadFixture(t)
	first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	require.NoError(err)
	source.query.Results = nil
	updated := paragraph("transcript-1", "Test Speaker: old meeting edited", false)
	source.blocks["transcript-1"] = &updated
	second, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat", Limit: 1, CreatedAfter: imp.now().Add(24 * time.Hour)})
	require.NoError(err)
	assert.Equal(int64(1), second.MeetingsUpdated)
	state := lastNotionState(t, st, first.SourceID)
	assert.Contains(state.Known, "meeting-1")
}
func TestImporterDiscoveryCheckpointSurvivesCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, source, imp := broadFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := ImportOptions{Identifier: "work", TokenType: "pat", Progress: func(line string) {
		if line == "Notion discovery checkpoint saved" {
			cancel()
		}
	}}
	sum, err := imp.Import(ctx, opts)
	require.ErrorIs(err, context.Canceled)
	cp, err := st.GetLatestCheckpointedSyncByType(t.Context(), sum.SourceID, SourceType)
	require.NoError(err)
	var state syncState
	require.NoError(json.Unmarshal([]byte(cp.CursorBefore.String), &state))
	assert.Contains(state.Admissions, "meeting-1")
	source.query.Results = nil
	next, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	require.NoError(err)
	assert.Equal(int64(1), next.MeetingsAdded)
}
func TestImporterCancellationAfterUpsertReplaysIdempotently(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, _, imp := broadFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sum, err := imp.Import(ctx, ImportOptions{Identifier: "work", TokenType: "pat", Progress: func(line string) {
		if line == "imported Notion meeting" {
			cancel()
		}
	}})
	require.ErrorIs(err, context.Canceled)
	assert.Equal(int64(1), sum.MeetingsAdded)
	next, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	require.NoError(err)
	assert.Zero(next.MeetingsAdded)
	assert.Zero(next.MeetingsUpdated)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&n))
	assert.Equal(1, n)
}
func TestForbiddenScopeDoesNotStallFreshSweep(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, source, imp := broadFixture(t)
	source.query.Results = nil
	source.childrenErrs = map[string]error{"root/": &APIError{Kind: ErrReadContent, Status: 403}}
	noSearch := false
	opts := ImportOptions{Identifier: "work", TokenType: "pat", RootPageIDs: []string{"root"}, SearchAccessible: &noSearch}
	for range 2 {
		sum, err := imp.Import(t.Context(), opts)
		require.NoError(err)
		assert.Positive(sum.CoverageReasons["inaccessible_subtree"])
	}
	delete(source.childrenErrs, "root/")
	source.children["root"] = map[string]*BlockPage{"": {Results: []Block{{Object: "block", ID: "meeting-1", Type: "meeting_notes"}}}}
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MeetingsAdded)
	assert.Contains(lastNotionState(t, st, sum.SourceID).Known, "meeting-1")
}
func TestRepeatedFullRetainsFrontier(t *testing.T) {
	require := require.New(t)

	st, source, imp := broadFixture(t)
	source.query.Results = nil
	imp.limits = defaultDiscoveryLimits()
	imp.limits.Requests = 1
	opts := ImportOptions{Identifier: "work", TokenType: "pat", RootPageIDs: []string{"root"}, Full: true}
	first, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	before := lastNotionState(t, st, first.SourceID)
	require.NotEmpty(before.Frontier)
	source.children["root"] = map[string]*BlockPage{
		"":     {HasMore: true, NextCursor: "next"},
		"next": {Results: []Block{{Object: "block", ID: "meeting-1", Type: "meeting_notes"}}},
	}
	for range 4 {
		_, err = imp.Import(t.Context(), opts)
		require.NoError(err)
	}
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&n))
	assert.Equal(t, 1, n)
}
func TestImporterUnknownIdentityFailsClosed(t *testing.T) {
	_, _, imp := broadFixture(t)
	sum, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.ErrorIs(t, err, ErrUnknownIdentity)
	assert.Zero(t, sum.MeetingsAdded)
}
func TestImporterDeferredMissingBlockKeepsArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, source, imp := broadFixture(t)
	first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	require.NoError(err)
	source.query.Results = nil
	source.blockErrs = map[string]error{"meeting-1": &APIError{Kind: ErrProvider, Status: 404}}
	next, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	require.NoError(err)
	assert.Positive(next.CoverageReasons["inaccessible_subtree"])
	state := lastNotionState(t, st, first.SourceID)
	assert.Equal("inaccessible_subtree", state.Known["meeting-1"].FetchError)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&n))
	assert.Equal(1, n)
}
func TestImporterLaterFailedRunDoesNotHideRecoverableProgress(t *testing.T) {
	require := require.New(t)

	st, source, imp := broadFixture(t)
	source.searches = func(context.Context, string) (*DiscoveryPage, error) { return nil, ErrUnauthorized }
	first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	require.ErrorIs(err, ErrUnauthorized)
	cp, err := st.GetLatestCheckpointedSyncByType(t.Context(), first.SourceID, SourceType)
	require.NoError(err)
	var saved syncState
	require.NoError(json.Unmarshal([]byte(cp.CursorBefore.String), &saved))
	require.Contains(saved.Admissions, "meeting-1")
	source.searches = func(context.Context, string) (*DiscoveryPage, error) { return nil, errors.New("synthetic transient") }
	source.query.Results = nil
	next, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	require.NoError(err)
	assert.Equal(t, int64(1), next.MeetingsAdded)
}

func TestImporterAbsentOptionalSectionsDoNotImplyCoverageGap(t *testing.T) {
	_, source, imp := broadFixture(t)
	meeting := hydrationMeeting()
	meeting.MeetingNotes.Children.SummaryBlockID = ""
	meeting.MeetingNotes.Children.NotesBlockID = ""
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	noSearch := false
	sum, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat", SearchAccessible: &noSearch})
	require.NoError(t, err)
	assert.NotContains(t, sum.CoverageReasons, "incomplete_sections")
	assert.False(t, sum.PartialCoverage)
}

type exhaustingAdmissionSource struct {
	*discoveryFixture

	knownReads int
}

func (s *exhaustingAdmissionSource) RetrieveBlock(ctx context.Context, id string) (*Block, error) {
	if id == "new" {
		for {
			if err := consumeAttempt(ctx); err != nil {
				return nil, err
			}
		}
	}
	s.knownReads++
	return s.discoveryFixture.RetrieveBlock(ctx, id)
}
func TestNewAdmissionsDoNotStarveOldRefresh(t *testing.T) {
	_, source, imp := broadFixture(t)
	_, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	require.NoError(t, err)
	source.query.Results = []MeetingNote{{ID: "new", CreatedTime: "2026-08-29T12:00:00Z"}}
	starving := &exhaustingAdmissionSource{discoveryFixture: source}
	imp.client = starving
	imp.limits.Attempts = 4
	for range 2 {
		_, err = imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
		require.NoError(t, err)
	}
	assert.Positive(t, starving.knownReads, "old refresh must receive a turn across two exhausted admission passes")
}

type headerCountingSource struct {
	*discoveryFixture

	reads map[string]int
}

func (s *headerCountingSource) RetrieveBlock(ctx context.Context, id string) (*Block, error) {
	if strings.HasPrefix(id, "known-") {
		s.reads[id]++
	}
	return s.discoveryFixture.RetrieveBlock(ctx, id)
}
func TestKnownRefreshActuallyVisitsEntireArchivedCycle(t *testing.T) {
	for _, count := range []int{0, 1, 31, 32, 33, 65} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st, source, imp := broadFixture(t)
			source.query.Results = nil
			ids := []string{}
			for i := 0; i < count; i++ {
				m := hydrationMeeting()
				m.ID = fmt.Sprintf("known-%03d", i)
				installMeetingHeader(t, source.fakeHydrationSource, m)
				ids = append(ids, m.ID)
			}
			noSearch := false
			first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat", MeetingBlockIDs: ids, SearchAccessible: &noSearch})
			require.NoError(err)
			assert.Equal(int64(count), first.MeetingsAdded)
			counter := &headerCountingSource{discoveryFixture: source, reads: map[string]int{}}
			imp.client = counter
			for pass := 0; pass < (count+31)/32; pass++ {
				_, err = imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat", SearchAccessible: &noSearch})
				require.NoError(err)
			}
			got := []string{}
			for id := range counter.reads {
				got = append(got, id)
			}
			assert.ElementsMatch(ids, got)
			var archived int
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&archived))
			assert.Equal(count, archived)
		})
	}
}

func TestLimitCountsOnlyNewSelectedAdmissions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, source, imp := broadFixture(t)
	noSearch := false
	opts := ImportOptions{Identifier: "work", TokenType: "pat", SearchAccessible: &noSearch}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	results := []MeetingNote{}
	for _, id := range []string{"unrelated", "meeting-1", "new-1", "new-2"} {
		m := hydrationMeeting()
		m.ID = id
		if id == "unrelated" {
			m.CreatedBy.ID = "other"
		}
		installMeetingHeader(t, source.fakeHydrationSource, m)
		results = append(results, m)
	}
	source.query.Results = results
	opts.Limit = 1
	first, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)
	state := lastNotionState(t, st, first.SourceID)
	assert.Contains(state.Admissions, "new-2")
	assert.NotContains(state.Known, "unrelated")
	assert.Contains(state.Known, "meeting-1")
	next, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(1), next.MeetingsAdded)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&count))
	assert.Equal(3, count)
}

func TestImporterVisitsDueTranscriptOutsideKnownRefreshWindow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, source, imp := broadFixture(t)
	source.query.Results = nil
	meeting := hydrationMeeting()
	meeting.ID = "known-zzz"
	meeting.MeetingNotes.Children.TranscriptBlockID = ""
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	now := imp.now()
	state := newSyncState()
	for i := range 40 {
		state.Known[fmt.Sprintf("known-%03d", i)] = knownMeeting{}
	}
	state.Known[meeting.ID] = knownMeeting{}
	state.Pending = []pendingTranscript{{BlockID: meeting.ID, NextAttemptAt: now.Add(-time.Hour).Format(time.RFC3339), RetryUntil: now.Add(time.Hour).Format(time.RFC3339)}}
	state.initialize()
	cursor, err := state.marshal()
	require.NoError(err)
	src, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "work")
	require.NoError(err)
	run, err := st.StartSyncContext(t.Context(), src.ID, SourceType)
	require.NoError(err)
	require.NoError(st.CompleteSyncContext(t.Context(), run, cursor))
	counter := &headerCountingSource{discoveryFixture: source, reads: map[string]int{}}
	imp.client = counter
	noSearch := false
	_, err = imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat", SearchAccessible: &noSearch})
	require.NoError(err)
	assert.Positive(counter.reads[meeting.ID])
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages WHERE source_message_id = ?", meeting.ID).Scan(&count))
	assert.Equal(1, count)
}

func TestOversizedEvidenceDefersOneMeetingAndContinues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, source, imp := broadFixture(t)
	large := hydrationMeeting()
	large.ID = "oversized"
	installMeetingHeader(t, source.fakeHydrationSource, large)
	text := strings.Repeat("a", 12<<20)
	for _, id := range []string{"summary-1", "notes-1"} {
		block := paragraph(id, text, false)
		source.blocks[id] = &block
	}
	next := hydrationMeeting()
	next.ID = "independent"
	next.MeetingNotes.Children.SummaryBlockID = ""
	next.MeetingNotes.Children.NotesBlockID = ""
	installMeetingHeader(t, source.fakeHydrationSource, next)
	source.query.Results = []MeetingNote{large, next}
	noSearch := false
	result, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat", SearchAccessible: &noSearch})
	require.NoError(err)
	assert.Positive(result.CoverageReasons["evidence_limit"])
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages WHERE source_message_id = ?", next.ID).Scan(&count))
	assert.Equal(1, count)
	state, err := imp.loadState(t.Context(), result.SourceID)
	require.NoError(err)
	require.NotEmpty(state.Deferred)
	assert.Equal("evidence_limit", state.Deferred[0].Reason)
}

func TestDeferredRootRetriesDoNotPreventFreshQueries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, source, imp := broadFixture(t)
	queries := 0
	source.windows = func(context.Context, int, *MeetingFilter) (*QueryResult, error) {
		queries++
		return &QueryResult{}, nil
	}
	source.childrenErrs = map[string]error{
		"root-a/": ErrReadContent,
		"root-b/": ErrReadContent,
		"root-c/": ErrReadContent,
	}
	imp.limits.Requests = 2
	now := imp.now()
	noSearch := false
	opts := ImportOptions{Identifier: "work", TokenType: "pat", RootPageIDs: []string{"root-a", "root-b", "root-c"}, SearchAccessible: &noSearch, Full: true}
	for i := range 12 {
		imp.now = func() time.Time { return now.Add(time.Duration(i) * 7 * time.Hour) }
		_, err := imp.Import(t.Context(), opts)
		require.NoError(err)
		if queries > 1 {
			break
		}
	}
	assert.Greater(queries, 1, "a fresh query must run even when deferred roots keep failing")
}

func TestKnownRefreshRespectsBacklogRetryCadence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := broadFixture(t)
	source.query.Results = nil
	now := imp.now()
	state := newSyncState()
	state.Known["known-backlog"] = knownMeeting{}
	deadline := now.Add(24 * time.Hour).Format(time.RFC3339)
	for i := range 200 {
		state.Pending = append(state.Pending, pendingTranscript{BlockID: fmt.Sprintf("other-%03d", i), NextAttemptAt: now.Add(time.Hour).Format(time.RFC3339), RetryUntil: deadline})
	}
	state.PendingBacklog = []pendingTranscript{{BlockID: "known-backlog", NextAttemptAt: now.Add(2 * time.Hour).Format(time.RFC3339), RetryUntil: deadline}}
	state.initialize()
	cursor, err := state.marshal()
	require.NoError(err)
	src, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "work")
	require.NoError(err)
	run, err := st.StartSyncContext(t.Context(), src.ID, SourceType)
	require.NoError(err)
	require.NoError(st.CompleteSyncContext(t.Context(), run, cursor))
	counter := &headerCountingSource{discoveryFixture: source, reads: map[string]int{}}
	imp.client = counter
	noSearch := false
	_, err = imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat", SearchAccessible: &noSearch})
	require.NoError(err)
	assert.Zero(counter.reads["known-backlog"])
	recovered, err := imp.loadState(t.Context(), src.ID)
	require.NoError(err)
	require.Len(recovered.PendingBacklog, 1)
	assert.Equal("known-backlog", recovered.PendingBacklog[0].BlockID)
}
