package notionmeetings

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"hegel.dev/go/hegel"
)

func TestCursorRejectsInvalidTraversalState(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers(1, 100))
		for name, mutate := range map[string]func(*syncState){
			"negative offset":     func(s *syncState) { s.Frontier = []scopeTask{{Kind: "database", ID: "db", Offset: -n}} },
			"negative depth":      func(s *syncState) { s.Frontier = []scopeTask{{Kind: "blocks", ID: "page", Depth: -n}} },
			"unknown kind":        func(s *syncState) { s.Frontier = []scopeTask{{Kind: fmt.Sprintf("unsupported%d", n), ID: "id"}} },
			"blank ID":            func(s *syncState) { s.Frontier = []scopeTask{{Kind: "blocks", ID: " "}} },
			"reversed window":     func(s *syncState) { s.Windows = []queryWindow{{From: time.Unix(int64(n)*60, 0), To: time.Unix(0, 0)}} },
			"duplicate admission": func(s *syncState) { s.Admissions = []string{"same", "same"} },
			"bad deferred": func(s *syncState) {
				s.Deferred = []deferredScope{{Task: &scopeTask{Kind: "search"}, Reason: "transient_error"}}
			},
			"invalid lane": func(s *syncState) { s.NextLane = 4 + n },
		} {
			s := newSyncState()
			mutate(&s)
			Assert.Error(ht, s.validate(), name)
		}
		s := newSyncState()
		s.Frontier = []scopeTask{{Kind: "database", ID: "db", Offset: n}}
		Require.NoError(ht, s.validate())
	}, hegel.WithTestCases(32), hegel.WithDatabase(""))
}

func TestImporterRejectsInvalidStoredCursorBeforeProviderRequests(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st, source, imp := broadFixture(t)
	src, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "work")
	require.NoError(err)
	state := newSyncState()
	state.Frontier = []scopeTask{{Kind: "database", ID: "private-id", Offset: -1}}
	raw, err := json.Marshal(state)
	require.NoError(err)
	runID, err := st.StartSyncContext(t.Context(), src.ID, SourceType)
	require.NoError(err)
	require.NoError(st.CompleteSyncContext(t.Context(), runID, string(raw)))
	source.windows = func(context.Context, int, *MeetingFilter) (*QueryResult, error) {
		Assert.Fail(t, "invalid saved state must be rejected before discovery")
		return nil, nil
	}
	_, err = imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	require.Error(err)
	assert.Contains(err.Error(), "cursor")
	assert.NotContains(err.Error(), "private-id")
}

func TestOversizedContinuationRetainsReplayableCheckpoint(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st, source, imp := broadFixture(t)
	source.query.Results = nil
	source.searches = func(context.Context, string) (*DiscoveryPage, error) {
		return &DiscoveryPage{Object: "list", HasMore: true, NextCursor: strings.Repeat("opaque", (2<<20)/6+1)}, nil
	}
	sum, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	require.NoError(err)
	assert.Positive(sum.CoverageReasons["cursor_budget"])
	active, err := st.GetActiveSync(sum.SourceID)
	require.ErrorIs(err, store.ErrSyncRunNotFound)
	assert.Nil(active)
	saved := lastNotionState(t, st, sum.SourceID)
	require.NoError(saved.validateTransient())
	require.NotEmpty(saved.Deferred)
	require.NotNil(saved.Deferred[0].Task)
	assert.Empty(saved.Deferred[0].Task.Cursor)
	source.searches = func(context.Context, string) (*DiscoveryPage, error) {
		return &DiscoveryPage{Object: "list", Results: []DiscoveryObject{{Object: "page", ID: "root"}}}, nil
	}
	source.children["root"] = map[string]*BlockPage{"": {Results: []Block{{Object: "block", ID: "meeting-1", Type: "meeting_notes"}}}}
	next, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	require.NoError(err)
	assert.Equal(int64(1), next.MeetingsAdded)
}

func TestOversizedRootConfigurationFailsBeforeStartingGeneration(t *testing.T) {
	st, _, imp := broadFixture(t)
	roots := make([]string, 1001)
	for i := range roots {
		roots[i] = fmt.Sprintf("root%d", i)
	}
	noSearch := false
	_, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat", SearchAccessible: &noSearch, RootPageIDs: roots})
	Require.Error(t, err)
	var count int
	Require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sync_runs").Scan(&count))
	Assert.Zero(t, count)
}

func failNotionSyncUpdates(t *testing.T, st *store.Store, status string) func() {
	t.Helper()
	condition := "TRUE"
	if status != "" {
		condition = "NEW.status = '" + status + "'"
	}
	if st.IsPostgreSQL() {
		_, err := st.DB().ExecContext(t.Context(), `CREATE FUNCTION fail_discovery_checkpoint() RETURNS trigger AS $$ BEGIN IF `+condition+` THEN RAISE EXCEPTION 'synthetic checkpoint write rejected'; END IF; RETURN NEW; END; $$ LANGUAGE plpgsql; CREATE TRIGGER fail_discovery_checkpoint BEFORE UPDATE ON sync_runs FOR EACH ROW EXECUTE FUNCTION fail_discovery_checkpoint();`)
		Require.NoError(t, err)
		return func() {
			_, err := st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_discovery_checkpoint ON sync_runs; DROP FUNCTION fail_discovery_checkpoint()`)
			Require.NoError(t, err)
		}
	}
	_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_discovery_checkpoint BEFORE UPDATE ON sync_runs WHEN `+condition+` BEGIN SELECT RAISE(ABORT,'synthetic checkpoint write rejected'); END`)
	Require.NoError(t, err)
	return func() {
		_, err := st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_discovery_checkpoint`)
		Require.NoError(t, err)
	}
}
func TestCheckpointAndCompletionFailuresPreserveUsableCursor(t *testing.T) {
	for _, status := range []string{"running", "completed", ""} {
		t.Run(status, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)

			st, source, imp := broadFixture(t)
			var restore func()
			source.windows = func(context.Context, int, *MeetingFilter) (*QueryResult, error) {
				if restore == nil {
					restore = failNotionSyncUpdates(t, st, status)
				}
				return source.query, nil
			}
			sum, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
			require.Error(err)
			assert.Contains(err.Error(), "synthetic checkpoint write rejected")
			var rowStatus, cursor string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT status,cursor_before FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1"), sum.SourceID).Scan(&rowStatus, &cursor))
			var saved syncState
			require.NoError(json.Unmarshal([]byte(cursor), &saved))
			require.NoError(saved.validate())
			assert.NotEmpty(cursor)
			if status != "" {
				assert.Equal("failed", rowStatus)
			} else {
				assert.Equal("running", rowStatus)
				assert.GreaterOrEqual(strings.Count(err.Error(), "synthetic checkpoint write rejected"), 2)
			}
			require.NotNil(restore)
			restore()
			source.windows = nil
			next, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
			require.NoError(err)
			var count int
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&count))
			assert.Equal(1, count)
			assert.LessOrEqual(next.MeetingsAdded, int64(1))
		})
	}
}

func TestRepairedScopeDebtIsSeparateFromHistoricalDiagnostics(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st, source, imp := broadFixture(t)
	source.query.Results = nil
	noSearch := false
	opts := ImportOptions{Identifier: "work", TokenType: "pat", RootPageIDs: []string{"root"}, SearchAccessible: &noSearch}
	source.childrenErrs = map[string]error{"root/": &APIError{Kind: ErrReadContent, Status: 403}}
	first, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Positive(first.CoverageReasons["inaccessible_subtree"])
	before := lastNotionState(t, st, first.SourceID)
	require.NotEmpty(before.Deferred)
	delete(source.childrenErrs, "root/")
	source.children["root"] = map[string]*BlockPage{"": {}}
	next, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.NotContains(next.CoverageReasons, "inaccessible_subtree")
	assert.Zero(next.WorkRemaining)
	after := lastNotionState(t, st, first.SourceID)
	assert.Empty(after.Deferred)
	assert.GreaterOrEqual(after.Coverage.DiagnosticCount, before.Coverage.DiagnosticCount)
}

func TestCursorEpochSelectionIsSourceAndTypeScoped(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st, _, imp := broadFixture(t)
	src, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "work")
	require.NoError(err)
	other, err := st.GetOrCreateSource(SourceType, "other")
	require.NoError(err)
	makeRun := func(source int64, typ, marker, status string) {
		state := newSyncState()
		state.Frontier = []scopeTask{{Kind: "search", Cursor: marker}}
		raw, err := state.marshal()
		Require.NoError(t, err)
		id, err := st.StartSyncContext(t.Context(), source, typ)
		Require.NoError(t, err)
		if status == "completed" {
			Require.NoError(t, st.CompleteSyncContext(t.Context(), id, raw))
		} else if status == "checkpoint" {
			Require.NoError(t, st.FailSyncWithCheckpointContext(t.Context(), id, "synthetic", &store.Checkpoint{PageToken: raw}))
		} else {
			Require.NoError(t, st.FailSyncContext(t.Context(), id, "no checkpoint"))
		}
	}
	makeRun(src.ID, SourceType, "old-success", "completed")
	makeRun(src.ID, SourceType, "recoverable", "checkpoint")
	makeRun(src.ID, SourceType, "uncheckpointed", "failed")
	makeRun(src.ID, "different-type", "wrong-type", "completed")
	makeRun(other.ID, SourceType, "wrong-source", "checkpoint")
	state, err := imp.loadState(t.Context(), src.ID)
	require.NoError(err)
	require.Len(state.Frontier, 1)
	assert.Equal("recoverable", state.Frontier[0].Cursor)
	makeRun(src.ID, SourceType, "new-success", "completed")
	makeRun(src.ID, SourceType, "later-uncheckpointed", "failed")
	state, err = imp.loadState(t.Context(), src.ID)
	require.NoError(err)
	require.Len(state.Frontier, 1)
	assert.Equal("new-success", state.Frontier[0].Cursor)
}

func TestCursorDefaultBoundsRetainDurableKnownIDs(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	limits := defaultDiscoveryLimits()
	for _, kind := range []string{"frontier", "windows", "seen", "pending", "diagnostics"} {
		t.Run(kind, func(t *testing.T) {
			s := newSyncState()
			switch kind {
			case "frontier":
				for i := 0; i <= limits.Frontier; i++ {
					s.Frontier = append(s.Frontier, scopeTask{Kind: "blocks", ID: fmt.Sprint(i)})
				}
			case "windows":
				s.Windows = make([]queryWindow, limits.Frontier+1)
			case "seen":
				for i := 0; i <= limits.Scopes; i++ {
					s.Seen[fmt.Sprint(i)] = true
				}
			case "pending":
				s.Admissions = make([]string, limits.Pending+1)
			case "diagnostics":
				s.Coverage.Diagnostics = make([]coverageDiagnostic, limits.Diagnostics+1)
			}
			Require.Error(t, s.validateTransient())
		})
	}
	s := newSyncState()
	for i := 0; i < 10000; i++ {
		s.Known[fmt.Sprint(i)] = knownMeeting{LastEditedTime: strings.Repeat("durable", 40)}
	}
	s.Admissions = []string{"next"}
	raw, err := s.marshal()
	require.NoError(err)
	var restored syncState
	require.NoError(json.Unmarshal([]byte(raw), &restored))
	assert.Equal(s.Known, restored.Known)
	assert.Equal(s.Admissions, restored.Admissions)
	for range 101 {
		s.reason("transient_error", "meeting")
	}
	assert.Len(s.Coverage.Diagnostics, 100)
	assert.Equal(101, s.Coverage.DiagnosticCount)
}

func TestScopeFingerprintEquivalenceAndSeparation(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.Text())
		identity := Identity{UserID: "human", PrincipalID: "bot", WorkspaceID: "workspace", Provenance: "pat_owner"}
		after := time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)
		opts := ImportOptions{TokenType: " PAT ", RootPageIDs: []string{"root", id}, CreatedAfter: after}
		equivalent := opts
		equivalent.TokenType = "pat"
		equivalent.RootPageIDs = []string{id, "root", "root"}
		equivalent.CreatedAfter = after.In(time.FixedZone("equivalent", -8*3600))
		enabled := true
		equivalent.SearchAccessible = &enabled
		equivalent.Full = true
		equivalent.Limit = 1
		equivalent.AccountEmail = "directory@example.com"
		fingerprint := scopeFingerprint("source", identity, opts)
		Assert.Equal(ht, fingerprint, scopeFingerprint("source", identity, equivalent))
		for _, change := range []Identity{{UserID: "other", PrincipalID: "bot", WorkspaceID: "workspace", Provenance: "pat_owner"}, {UserID: "human", PrincipalID: "other", WorkspaceID: "workspace", Provenance: "pat_owner"}, {UserID: "human", PrincipalID: "bot", WorkspaceID: "other", Provenance: "pat_owner"}} {
			Assert.NotEqual(ht, fingerprint, scopeFingerprint("source", change, opts))
		}
		Assert.NotEqual(ht, fingerprint, scopeFingerprint("other", identity, opts))
		equivalent.RootPageIDs = nil
		equivalent.RootDataSourceIDs = opts.RootPageIDs
		Assert.NotEqual(ht, fingerprint, scopeFingerprint("source", identity, equivalent))
	}, hegel.WithTestCases(64), hegel.WithDatabase(""))
}

func TestDeferredQueryDebtDeduplicatesAndResolves(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	run, source := schedulerFixture(t)
	window := queryWindow{From: run.now, To: run.now.Add(time.Minute)}
	source.windows = func(context.Context, int, *MeetingFilter) (*QueryResult, error) {
		return &QueryResult{HasMore: true}, nil
	}
	for range 3 {
		run.state.Windows = []queryWindow{window}
		changed, err := run.stepQuery(t.Context())
		require.NoError(err)
		require.True(changed)
	}
	require.Len(run.state.Deferred, 1)
	future := run.state.Deferred[0].NextAttemptAt
	run.state.activateDeferred(future.Add(-time.Nanosecond), run.limits)
	assert.Empty(run.state.Windows)
	require.Len(run.state.Deferred, 1)
	run.state.activateDeferred(future, run.limits)
	require.Len(run.state.Windows, 1)
	source.windows = func(context.Context, int, *MeetingFilter) (*QueryResult, error) { return &QueryResult{}, nil }
	changed, err := run.stepQuery(t.Context())
	require.NoError(err)
	assert.True(changed)
	assert.Empty(run.state.Deferred)
}

func TestPrecommitArchiveFailureRetainsAdmission(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st, _, imp := broadFixture(t)
	if st.IsPostgreSQL() {
		_, err := st.DB().ExecContext(t.Context(), `CREATE FUNCTION fail_discovery_archive() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'synthetic archive failure'; END; $$ LANGUAGE plpgsql; CREATE TRIGGER fail_discovery_archive BEFORE INSERT ON messages FOR EACH ROW EXECUTE FUNCTION fail_discovery_archive()`)
		require.NoError(err)
	} else {
		_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_discovery_archive BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'synthetic archive failure'); END`)
		require.NoError(err)
	}
	sum, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	require.Error(err)
	assert.Zero(sum.MeetingsAdded)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&count))
	assert.Zero(count)
	cp, err := st.GetLatestCheckpointedSyncByType(t.Context(), sum.SourceID, SourceType)
	require.NoError(err)
	var state syncState
	require.NoError(json.Unmarshal([]byte(cp.CursorBefore.String), &state))
	assert.Contains(state.Admissions, "meeting-1")
	assert.NotContains(state.Known, "meeting-1")
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_discovery_archive ON messages; DROP FUNCTION fail_discovery_archive()`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_discovery_archive`)
	}
	require.NoError(err)
	next, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	require.NoError(err)
	assert.Equal(int64(1), next.MeetingsAdded)
}

func TestMeetingIdentitySurvivesMovesAndSeparateSources(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		kind := hegel.Draw(ht, hegel.Integers(0, 3))
		st, source, imp := broadFixture(t)
		first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
		Require.NoError(ht, err)
		var id int64
		Require.NoError(ht, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
		meeting := hydrationMeeting()
		parents := []Parent{{Type: "page_id", PageID: "moved"}, {Type: "data_source_id", DataSourceID: "moved"}, {Type: "block_id", BlockID: "moved"}, {Type: "agent_id", AgentID: "moved"}}
		meeting.Parent = parents[kind]
		meeting.LastEditedTime = "2026-08-29T11:00:00Z"
		installMeetingHeader(t, source.fakeHydrationSource, meeting)
		source.query.Results = nil
		updated := paragraph("transcript-1", "MOVE_SCOPED_TRANSCRIPT", false)
		source.blocks[updated.ID] = &updated
		second, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
		Require.NoError(ht, err)
		Assert.Equal(ht, int64(1), second.MeetingsUpdated)
		raw, err := st.GetMessageRaw(id)
		Require.NoError(ht, err)
		Assert.Contains(ht, string(raw), "MOVE_SCOPED_TRANSCRIPT")
		Assert.Contains(ht, string(raw), `"`+meeting.Parent.Type+`"`)
		third, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
		Require.NoError(ht, err)
		Assert.Zero(ht, third.MeetingsAdded)
		Assert.Zero(ht, third.MeetingsUpdated)
		sameRaw, err := st.GetMessageRaw(id)
		Require.NoError(ht, err)
		Assert.Equal(ht, raw, sameRaw)
		_, err = st.GetOrCreateSource(SourceType, "other")
		Require.NoError(ht, err)
		source.query.Results = []MeetingNote{meeting}
		other, err := imp.Import(t.Context(), ImportOptions{Identifier: "other", TokenType: "pat"})
		Require.NoError(ht, err)
		Assert.NotEqual(ht, first.SourceID, other.SourceID)
		Assert.Equal(ht, int64(1), other.MeetingsAdded)
		var count int
		Require.NoError(ht, st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&count))
		Assert.Equal(ht, 2, count)
	}, hegel.WithTestCases(4), hegel.WithDatabase(""))
}

func TestTransientByteBudgetIncludesOpaqueCursorEncoding(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		cursor := hegel.Draw(ht, hegel.Text())
		s := newSyncState()
		s.Frontier = []scopeTask{{Kind: "search", Cursor: cursor}}
		transient := s
		transient.Known = nil
		transient.TranscriptRetryUntil = nil
		encoded, err := jsonv2.Marshal(transient)
		Require.NoError(ht, err)
		for _, delta := range []int{-1, 0, 1} {
			limits := defaultDiscoveryLimits()
			limits.Bytes = len(encoded) + delta
			err := s.validateTransientWith(limits)
			if delta < 0 {
				Require.Error(ht, err)
			} else {
				Require.NoError(ht, err)
			}
		}
	}, hegel.WithTestCases(64), hegel.WithDatabase(""))
}
