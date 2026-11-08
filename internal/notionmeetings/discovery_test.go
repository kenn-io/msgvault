package notionmeetings

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

type discoveryFixture struct {
	*fakeImportSource

	searches  func(context.Context, string) (*DiscoveryPage, error)
	rows      func(context.Context, string, string) (*DiscoveryPage, error)
	windows   func(context.Context, int, *MeetingFilter) (*QueryResult, error)
	databases map[string]*Database
}

func (f *discoveryFixture) RetrieveSelf(context.Context) (*User, error) {
	return &User{Object: "user", ID: "bot", Type: "bot", Bot: BotInfo{Owner: BotOwner{Type: "user", User: UserRef{ID: "creator-1"}}}}, nil
}
func (f *discoveryFixture) QueryMeetingNotesFiltered(ctx context.Context, n int, filter *MeetingFilter) (*QueryResult, error) {
	if f.windows != nil {
		return f.windows(ctx, n, filter)
	}
	return f.QueryMeetingNotes(ctx, n)
}
func (f *discoveryFixture) Search(ctx context.Context, cursor string) (*DiscoveryPage, error) {
	if f.searches != nil {
		return f.searches(ctx, cursor)
	}
	return &DiscoveryPage{Object: "list"}, nil
}
func (f *discoveryFixture) QueryDataSource(ctx context.Context, id, cursor string) (*DiscoveryPage, error) {
	if f.rows != nil {
		return f.rows(ctx, id, cursor)
	}
	return &DiscoveryPage{Object: "list"}, nil
}
func (f *discoveryFixture) RetrieveDatabase(_ context.Context, id string) (*Database, error) {
	if db := f.databases[id]; db != nil {
		return db, nil
	}
	return nil, errors.New("synthetic missing database")
}
func schedulerFixture(t *testing.T) (*discoveryRun, *discoveryFixture) {
	t.Helper()
	_, fake, _ := newImporterFixture(t)
	source := &discoveryFixture{fakeImportSource: fake}
	state := newSyncState()
	run := &discoveryRun{state: &state, client: source, source: source, limits: defaultDiscoveryLimits(), now: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	return run, source
}
func TestQueueBackpressureReplaysEveryCandidate(t *testing.T) {
	run, source := schedulerFixture(t)
	run.limits.Frontier = 2
	source.children = map[string]map[string]*BlockPage{}
	source.searches = func(ctx context.Context, cursor string) (*DiscoveryPage, error) {
		n := 0
		if cursor != "" {
			_, _ = fmt.Sscanf(cursor, "c%d", &n)
		}
		size := discoveryPageSize(ctx)
		end := min(n+size, 7)
		page := &DiscoveryPage{Object: "list", HasMore: end < 7}
		if page.HasMore {
			page.NextCursor = fmt.Sprintf("c%d", end)
		}
		for i := n; i < end; i++ {
			id := fmt.Sprintf("p%d", i)
			page.Results = append(page.Results, DiscoveryObject{Object: "page", ID: id})
			m := hydrationMeeting()
			m.ID = fmt.Sprintf("m%d", i)
			b := Block{Object: "block", ID: m.ID, Type: "meeting_notes"}
			source.children[id] = map[string]*BlockPage{"": {Results: []Block{b}}}
		}
		return page, nil
	}
	run.state.startSweep(ImportOptions{}, run.now)
	var admitted []string
	for passes := 0; passes < 30 && (run.state.Active || len(run.state.Deferred) > 0); passes++ {
		for steps := 0; steps < 30 && len(run.state.Frontier) > 0; steps++ {
			_, err := run.stepTraversal(t.Context())
			require.NoError(t, err)
			admitted = append(admitted, run.state.Admissions...)
			run.state.Admissions = nil
			assert.LessOrEqual(t, len(run.state.Frontier), run.limits.Frontier)
		}
		run.now = run.now.Add(7 * time.Hour)
		run.state.activateDeferred(run.now, run.limits)
		run.state.Active = len(run.state.Frontier) > 0
	}
	assert.ElementsMatch(t, []string{"m0", "m1", "m2", "m3", "m4", "m5", "m6"}, admitted)
}
func TestSaturatedMinuteAllowsOlderWindows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	run, source := schedulerFixture(t)
	start := run.now.Truncate(time.Minute)
	run.state.Windows = []queryWindow{{From: start, To: start.Add(time.Minute)}, {To: start}}
	source.windows = func(_ context.Context, _ int, f *MeetingFilter) (*QueryResult, error) {
		return &QueryResult{Results: []MeetingNote{{ID: "tie", CreatedTime: start.Format(time.RFC3339)}}, HasMore: true}, nil
	}
	progressed, err := run.stepQuery(t.Context())
	require.NoError(err)
	assert.True(progressed)
	require.Len(run.state.Windows, 1)
	assert.Equal(start, run.state.Windows[0].To)
	assert.Equal(1, run.state.Coverage.Reasons["query_cap"])
	assert.Contains(run.state.Admissions, "tie")
}
func TestDiscoveryFingerprintIsOrderIndependentAndAfterSensitive(t *testing.T) {
	identity := Identity{UserID: "human", PrincipalID: "bot"}
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.Text())
		opts := ImportOptions{RootPageIDs: []string{"fixed", id}}
		reordered := opts
		reordered.RootPageIDs = []string{id, "fixed", "fixed"}
		assert.Equal(ht, scopeFingerprint("source", identity, opts), scopeFingerprint("source", identity, reordered))
		reordered.CreatedAfter = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		assert.NotEqual(ht, scopeFingerprint("source", identity, opts), scopeFingerprint("source", identity, reordered))
	}, hegel.WithTestCases(64), hegel.WithDatabase(""))
}

func TestDataSourceCapOnEveryPageAndWikiDispatch(t *testing.T) {
	assert := assert.New(t)

	run, source := schedulerFixture(t)
	run.state.Frontier = []scopeTask{{Kind: "data_source", ID: "data"}}
	run.state.Seen["data_source:data"] = true
	source.children["page"] = map[string]*BlockPage{"": {Results: []Block{{Object: "block", ID: "meeting-1", Type: "meeting_notes"}}}}
	var queried []string
	source.rows = func(_ context.Context, id, cursor string) (*DiscoveryPage, error) {
		queried = append(queried, id+"/"+cursor)
		p := &DiscoveryPage{Object: "list"}
		if id == "data" {
			p.RequestStatus.Type = "incomplete"
			p.RequestStatus.IncompleteReason = "query_result_limit_reached"
			if cursor == "" {
				p.HasMore = true
				p.NextCursor = "next"
				p.Results = []DiscoveryObject{{Object: "data_source", ID: "wiki"}}
			} else {
				p.Results = []DiscoveryObject{{Object: "page", ID: "page"}}
			}
		}
		return p, nil
	}
	for len(run.state.Frontier) > 0 {
		_, err := run.stepTraversal(t.Context())
		require.NoError(t, err)
	}
	assert.Equal(2, run.state.Coverage.Reasons["data_source_cap"])
	assert.ElementsMatch([]string{"data/", "wiki/", "data/next"}, queried)
	assert.Equal([]string{"meeting-1"}, run.state.Admissions)
}
func TestChildDatabaseTraversesAllDataSourcesWithOneFrontierSlot(t *testing.T) {
	run, source := schedulerFixture(t)
	run.limits.Frontier = 1
	run.state.Frontier = []scopeTask{{Kind: "database", ID: "database"}}
	run.state.Seen["database:database"] = true
	source.databases = map[string]*Database{"database": {Object: "database", ID: "database", DataSources: []DiscoveryObject{{ID: "a"}, {ID: "b"}, {ID: "c"}}}}
	var queried []string
	source.rows = func(_ context.Context, id, _ string) (*DiscoveryPage, error) {
		queried = append(queried, id)
		return &DiscoveryPage{Object: "list"}, nil
	}
	for rounds := 0; rounds < 10 && (len(run.state.Frontier) > 0 || len(run.state.Deferred) > 0); rounds++ {
		for len(run.state.Frontier) > 0 {
			_, err := run.stepTraversal(t.Context())
			require.NoError(t, err)
		}
		run.now = run.now.Add(7 * time.Hour)
		run.state.activateDeferred(run.now, run.limits)
	}
	assert.ElementsMatch(t, []string{"a", "b", "c"}, queried)
}
func TestTraversalRepeatedCursorIsDeferredAndCycleDoesNotExpand(t *testing.T) {
	assert := assert.New(t)

	run, source := schedulerFixture(t)
	run.state.Frontier = []scopeTask{{Kind: "blocks", ID: "root"}}
	run.state.Seen["blocks:root"] = true
	source.children["root"] = map[string]*BlockPage{"": {Results: []Block{{Object: "block", ID: "root", Type: "child_page"}}, HasMore: true, NextCursor: "same"}, "same": {HasMore: true, NextCursor: "same"}}
	for len(run.state.Frontier) > 0 {
		_, err := run.stepTraversal(t.Context())
		require.NoError(t, err)
	}
	assert.Len(run.state.Seen, 1)
	assert.Equal(1, run.state.Coverage.Reasons["cursor_cycle"])
	assert.Len(run.state.Deferred, 1)
}
func TestQuerySplitRetainsOpenHistoricalTail(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		year := hegel.Draw(ht, hegel.Integers(1, 2025))
		month := hegel.Draw(ht, hegel.Integers(1, 12))
		day := hegel.Draw(ht, hegel.Integers(1, 28))
		at := time.Date(year, time.Month(month), day, 12, 0, 0, 0, time.UTC)
		upper := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
		parts, ok := (queryWindow{To: upper}).split([]MeetingNote{{CreatedTime: at.Format(time.RFC3339)}})
		require.True(ht, ok)
		require.Len(ht, parts, 2)
		assert.True(ht, parts[0].From.IsZero())
		assert.Equal(ht, upper, parts[1].To)
		assert.Equal(ht, parts[0].To, parts[1].From)
		assert.Equal(ht, at.Truncate(24*time.Hour), parts[0].To)
	}, hegel.WithTestCases(128), hegel.WithDatabase(""))
}

func TestBoundedTraversalCheckpointRoundTripPreservesAdmissions(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := 1 + int(hegel.Draw(ht, hegel.Integers(uint(0), ^uint(0)))%64)
		capacity := hegel.Draw(ht, hegel.Integers(1, 8))
		pending := hegel.Draw(ht, hegel.Integers(1, 8))
		source := &discoveryFixture{fakeImportSource: &fakeImportSource{fakeHydrationSource: completeHydrationSource(), query: &QueryResult{}}}
		state := newSyncState()
		limits := defaultDiscoveryLimits()
		limits.Frontier = capacity
		limits.Pending = pending
		run := &discoveryRun{state: &state, client: source, source: source, limits: limits, now: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)}
		source.searches = func(ctx context.Context, cursor string) (*DiscoveryPage, error) {
			from := 0
			if cursor != "" {
				_, _ = fmt.Sscanf(cursor, "p%d", &from)
			}
			end := min(from+discoveryPageSize(ctx), count)
			page := &DiscoveryPage{Object: "list", HasMore: end < count}
			if page.HasMore {
				page.NextCursor = fmt.Sprintf("p%d", end)
			}
			for index := from; index < end; index++ {
				id := fmt.Sprintf("page-%d", index)
				page.Results = append(page.Results, DiscoveryObject{Object: "page", ID: id})
				source.children[id] = map[string]*BlockPage{"": {Results: []Block{{Object: "block", ID: fmt.Sprintf("meeting-%d", index), Type: "meeting_notes"}}}}
			}
			return page, nil
		}
		state.Frontier = []scopeTask{{Kind: "search"}}
		state.Seen["search:"] = true
		got := map[string]bool{}
		for steps := 0; steps < 2048 && (len(state.Frontier) > 0 || len(state.Deferred) > 0); steps++ {
			_, err := run.stepTraversal(t.Context())
			require.NoError(ht, err)
			require.LessOrEqual(ht, len(state.Frontier), capacity)
			require.LessOrEqual(ht, len(state.Admissions), pending)
			for _, id := range state.Admissions {
				assert.False(ht, got[id], "candidate replay deduplicates archived IDs")
				got[id] = true
				state.Known[id] = knownMeeting{}
			}
			state.Admissions = nil
			encoded, err := state.marshal()
			require.NoError(ht, err)
			var restored syncState
			require.NoError(ht, json.Unmarshal([]byte(encoded), &restored))
			state = restored
			run.state = &state
			if len(state.Frontier) == 0 {
				run.now = run.now.Add(7 * time.Hour)
				state.activateDeferred(run.now, limits)
			}
		}
		want := map[string]bool{}
		for index := range count {
			want[fmt.Sprintf("meeting-%d", index)] = true
		}
		assert.Equal(ht, want, got)
	}, hegel.WithTestCases(32), hegel.WithDatabase(""))
}
