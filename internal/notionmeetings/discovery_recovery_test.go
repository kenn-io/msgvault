package notionmeetings

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			"negative root position": func(s *syncState) {
				s.Active = true
				s.SeedNext = -n
			},
		} {
			s := newSyncState()
			mutate(&s)
			require.Error(ht, s.validate(), name)
		}
		s := newSyncState()
		s.Frontier = []scopeTask{{Kind: "database", ID: "db", Offset: n}}
		require.NoError(ht, s.validate())
		s.SeedNext = n
		require.NoError(ht, s.validate())
	}, hegel.WithTestCases(32), hegel.WithDatabase(""))
}

func TestVersionFourCursorRemainsReadable(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	state := newSyncState()
	state.Version = 4
	state.Active = true
	state.Frontier = []scopeTask{{Kind: "blocks", ID: "root", Cursor: "next"}}
	raw, err := json.Marshal(state)
	must.NoError(err)
	var restored syncState
	must.NoError(json.Unmarshal(raw, &restored))
	must.NoError(restored.validate())
	must.Len(restored.Frontier, 1)
	checks.Equal("next", restored.Frontier[0].Cursor)
}

func TestImporterRejectsInvalidStoredCursorBeforeProviderRequests(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)

	st, source, imp := broadFixture(t)
	src, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "work")
	must.NoError(err)
	state := newSyncState()
	state.Frontier = []scopeTask{{Kind: "database", ID: "private-id", Offset: -1}}
	raw, err := json.Marshal(state)
	must.NoError(err)
	runID, err := st.StartSyncContext(t.Context(), src.ID, SourceType)
	must.NoError(err)
	must.NoError(st.CompleteSyncContext(t.Context(), runID, string(raw)))
	source.windows = func(context.Context, int, *MeetingFilter) (*QueryResult, error) {
		assert.Fail(t, "invalid saved state must be rejected before discovery")
		return nil, errors.New("unexpected discovery request")
	}
	_, err = imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	must.Error(err)
	checks.Contains(err.Error(), "cursor")
	checks.NotContains(err.Error(), "private-id")
}

func TestOversizedContinuationRetainsReplayableCheckpoint(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)

	st, source, imp := broadFixture(t)
	source.query.Results = nil
	source.searches = func(context.Context, string) (*DiscoveryPage, error) {
		return &DiscoveryPage{Object: "list", HasMore: true, NextCursor: strings.Repeat("opaque", (2<<20)/6+1)}, nil
	}
	sum, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	must.NoError(err)
	checks.Positive(sum.CoverageReasons["cursor_budget"])
	active, err := st.GetActiveSync(sum.SourceID)
	must.ErrorIs(err, store.ErrSyncRunNotFound)
	checks.Nil(active)
	saved := lastNotionState(t, st, sum.SourceID)
	must.NoError(saved.validateTransient())
	must.NotEmpty(saved.Deferred)
	must.NotNil(saved.Deferred[0].Task)
	checks.Empty(saved.Deferred[0].Task.Cursor)
	source.searches = func(context.Context, string) (*DiscoveryPage, error) {
		return &DiscoveryPage{Object: "list", Results: []DiscoveryObject{{Object: "page", ID: "root"}}}, nil
	}
	source.children["root"] = map[string]*BlockPage{"": {Results: []Block{{Object: "block", ID: "meeting-1", Type: "meeting_notes"}}}}
	next, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	must.NoError(err)
	checks.Equal(int64(1), next.MeetingsAdded)
}

func TestOversizedRootConfigurationFailsBeforeStartingGeneration(t *testing.T) {
	st, _, imp := broadFixture(t)
	roots := make([]string, 1001)
	for i := range roots {
		roots[i] = fmt.Sprintf("root%d", i)
	}
	noSearch := false
	_, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat", SearchAccessible: &noSearch, RootPageIDs: roots})
	require.Error(t, err)
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sync_runs").Scan(&count))
	assert.Zero(t, count)
}

func failNotionSyncUpdates(t *testing.T, st *store.Store, status string) func() {
	t.Helper()
	condition := "TRUE"
	if status != "" {
		condition = "NEW.status = '" + status + "'"
	}
	if st.IsPostgreSQL() {
		_, err := st.DB().ExecContext(t.Context(), `CREATE FUNCTION fail_discovery_checkpoint() RETURNS trigger AS $$ BEGIN IF `+condition+` THEN RAISE EXCEPTION 'synthetic checkpoint write rejected'; END IF; RETURN NEW; END; $$ LANGUAGE plpgsql; CREATE TRIGGER fail_discovery_checkpoint BEFORE UPDATE ON sync_runs FOR EACH ROW EXECUTE FUNCTION fail_discovery_checkpoint();`)
		require.NoError(t, err)
		return func() {
			_, err := st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_discovery_checkpoint ON sync_runs; DROP FUNCTION fail_discovery_checkpoint()`)
			require.NoError(t, err)
		}
	}
	_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_discovery_checkpoint BEFORE UPDATE ON sync_runs WHEN `+condition+` BEGIN SELECT RAISE(ABORT,'synthetic checkpoint write rejected'); END`)
	require.NoError(t, err)
	return func() {
		_, err := st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_discovery_checkpoint`)
		require.NoError(t, err)
	}
}
func TestCheckpointAndCompletionFailuresPreserveUsableCursor(t *testing.T) {
	for _, status := range []string{"running", "completed", ""} {
		t.Run(status, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)

			st, source, imp := broadFixture(t)
			var restore func()
			source.windows = func(context.Context, int, *MeetingFilter) (*QueryResult, error) {
				if restore == nil {
					restore = failNotionSyncUpdates(t, st, status)
				}
				return source.query, nil
			}
			sum, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
			must.Error(err)
			checks.Contains(err.Error(), "synthetic checkpoint write rejected")
			var rowStatus, cursor string
			must.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT status,cursor_before FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1"), sum.SourceID).Scan(&rowStatus, &cursor))
			var saved syncState
			must.NoError(json.Unmarshal([]byte(cursor), &saved))
			must.NoError(saved.validate())
			checks.NotEmpty(cursor)
			if status != "" {
				checks.Equal("failed", rowStatus)
			} else {
				checks.Equal("running", rowStatus)
				checks.GreaterOrEqual(strings.Count(err.Error(), "synthetic checkpoint write rejected"), 2)
			}
			must.NotNil(restore)
			restore()
			source.windows = nil
			next, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
			must.NoError(err)
			var count int
			must.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&count))
			checks.Equal(1, count)
			checks.LessOrEqual(next.MeetingsAdded, int64(1))
		})
	}
}

func TestRepairedScopeDebtIsSeparateFromHistoricalDiagnostics(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)

	st, source, imp := broadFixture(t)
	source.query.Results = nil
	noSearch := false
	opts := ImportOptions{Identifier: "work", TokenType: "pat", RootPageIDs: []string{"root"}, SearchAccessible: &noSearch}
	source.childrenErrs = map[string]error{"root/": &APIError{Kind: ErrReadContent, Status: 403}}
	first, err := imp.Import(t.Context(), opts)
	must.NoError(err)
	checks.Positive(first.CoverageReasons["inaccessible_subtree"])
	before := lastNotionState(t, st, first.SourceID)
	must.NotEmpty(before.Deferred)
	delete(source.childrenErrs, "root/")
	source.children["root"] = map[string]*BlockPage{"": {}}
	next, err := imp.Import(t.Context(), opts)
	must.NoError(err)
	checks.NotContains(next.CoverageReasons, "inaccessible_subtree")
	checks.Zero(next.WorkRemaining)
	after := lastNotionState(t, st, first.SourceID)
	checks.Empty(after.Deferred)
	checks.GreaterOrEqual(after.Coverage.DiagnosticCount, before.Coverage.DiagnosticCount)
}

func TestCursorEpochSelectionIsSourceAndTypeScoped(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)

	st, _, imp := broadFixture(t)
	src, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "work")
	must.NoError(err)
	other, err := st.GetOrCreateSource(SourceType, "other")
	must.NoError(err)
	makeRun := func(source int64, typ, marker, status string) {
		state := newSyncState()
		state.Frontier = []scopeTask{{Kind: "search", Cursor: marker}}
		raw, err := state.marshal()
		require.NoError(t, err)
		id, err := st.StartSyncContext(t.Context(), source, typ)
		require.NoError(t, err)
		switch status {
		case "completed":
			require.NoError(t, st.CompleteSyncContext(t.Context(), id, raw))
		case "checkpoint":
			require.NoError(t, st.FailSyncWithCheckpointContext(t.Context(), id, "synthetic", &store.Checkpoint{PageToken: raw}))
		default:
			require.NoError(t, st.FailSyncContext(t.Context(), id, "no checkpoint"))
		}
	}
	makeRun(src.ID, SourceType, "old-success", "completed")
	makeRun(src.ID, SourceType, "recoverable", "checkpoint")
	makeRun(src.ID, SourceType, "uncheckpointed", "failed")
	makeRun(src.ID, "different-type", "wrong-type", "completed")
	makeRun(other.ID, SourceType, "wrong-source", "checkpoint")
	state, err := imp.loadState(t.Context(), src.ID)
	must.NoError(err)
	must.Len(state.Frontier, 1)
	checks.Equal("recoverable", state.Frontier[0].Cursor)
	makeRun(src.ID, SourceType, "new-success", "completed")
	makeRun(src.ID, SourceType, "later-uncheckpointed", "failed")
	state, err = imp.loadState(t.Context(), src.ID)
	must.NoError(err)
	must.Len(state.Frontier, 1)
	checks.Equal("new-success", state.Frontier[0].Cursor)
}

func TestCursorDefaultBoundsRetainDurableKnownIDs(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)

	limits := defaultDiscoveryLimits()
	for _, kind := range []string{"frontier", "windows", "seen", "pending", "diagnostics"} {
		t.Run(kind, func(t *testing.T) {
			s := newSyncState()
			switch kind {
			case "frontier":
				for i := 0; i <= limits.Frontier; i++ {
					s.Frontier = append(s.Frontier, scopeTask{Kind: "blocks", ID: strconv.Itoa(i)})
				}
			case "windows":
				s.Windows = make([]queryWindow, limits.Frontier+1)
			case "seen":
				for i := 0; i <= limits.Scopes; i++ {
					s.Seen[strconv.Itoa(i)] = true
				}
			case "pending":
				s.Admissions = make([]string, limits.Pending+1)
			case "diagnostics":
				s.Coverage.Diagnostics = make([]coverageDiagnostic, limits.Diagnostics+1)
			}
			require.Error(t, s.validateTransient())
		})
	}
	s := newSyncState()
	for i := range 10000 {
		s.Known[strconv.Itoa(i)] = knownMeeting{LastEditedTime: strings.Repeat("durable", 40)}
	}
	s.Admissions = []string{"next"}
	raw, err := s.marshal()
	must.NoError(err)
	var restored syncState
	must.NoError(json.Unmarshal([]byte(raw), &restored))
	checks.Equal(s.Known, restored.Known)
	checks.Equal(s.Admissions, restored.Admissions)
	for range 101 {
		s.reason("transient_error", "meeting")
	}
	checks.Len(s.Coverage.Diagnostics, 100)
	checks.Equal(101, s.Coverage.DiagnosticCount)
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
		assert.Equal(ht, fingerprint, scopeFingerprint("source", identity, equivalent))
		for _, change := range []Identity{{UserID: "other", PrincipalID: "bot", WorkspaceID: "workspace", Provenance: "pat_owner"}, {UserID: "human", PrincipalID: "other", WorkspaceID: "workspace", Provenance: "pat_owner"}, {UserID: "human", PrincipalID: "bot", WorkspaceID: "other", Provenance: "pat_owner"}} {
			assert.NotEqual(ht, fingerprint, scopeFingerprint("source", change, opts))
		}
		assert.NotEqual(ht, fingerprint, scopeFingerprint("other", identity, opts))
		equivalent.RootPageIDs = nil
		equivalent.RootDataSourceIDs = opts.RootPageIDs
		assert.NotEqual(ht, fingerprint, scopeFingerprint("source", identity, equivalent))
	}, hegel.WithTestCases(64), hegel.WithDatabase(""))
}

func TestDeferredQueryDebtDeduplicatesAndResolves(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)

	run, source := schedulerFixture(t)
	window := queryWindow{From: run.now, To: run.now.Add(time.Minute)}
	source.windows = func(context.Context, int, *MeetingFilter) (*QueryResult, error) {
		return &QueryResult{HasMore: true}, nil
	}
	for range 3 {
		run.state.Windows = []queryWindow{window}
		changed, err := run.stepQuery(t.Context())
		must.NoError(err)
		must.True(changed)
	}
	must.Len(run.state.Deferred, 1)
	future := run.state.Deferred[0].NextAttemptAt
	run.state.activateDeferred(future.Add(-time.Nanosecond), run.limits)
	checks.Empty(run.state.Windows)
	must.Len(run.state.Deferred, 1)
	run.state.activateDeferred(future, run.limits)
	must.Len(run.state.Windows, 1)
	source.windows = func(context.Context, int, *MeetingFilter) (*QueryResult, error) { return &QueryResult{}, nil }
	changed, err := run.stepQuery(t.Context())
	must.NoError(err)
	checks.True(changed)
	checks.Empty(run.state.Deferred)
}

func TestPrecommitArchiveFailureRetainsAdmission(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)

	st, _, imp := broadFixture(t)
	if st.IsPostgreSQL() {
		_, err := st.DB().ExecContext(t.Context(), `CREATE FUNCTION fail_discovery_archive() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'synthetic archive failure'; END; $$ LANGUAGE plpgsql; CREATE TRIGGER fail_discovery_archive BEFORE INSERT ON messages FOR EACH ROW EXECUTE FUNCTION fail_discovery_archive()`)
		must.NoError(err)
	} else {
		_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_discovery_archive BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'synthetic archive failure'); END`)
		must.NoError(err)
	}
	sum, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	must.Error(err)
	checks.Zero(sum.MeetingsAdded)
	var count int
	must.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&count))
	checks.Zero(count)
	cp, err := st.GetLatestCheckpointedSyncByType(t.Context(), sum.SourceID, SourceType)
	must.NoError(err)
	var state syncState
	must.NoError(json.Unmarshal([]byte(cp.CursorBefore.String), &state))
	checks.Contains(state.Admissions, "meeting-1")
	checks.NotContains(state.Known, "meeting-1")
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_discovery_archive ON messages; DROP FUNCTION fail_discovery_archive()`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_discovery_archive`)
	}
	must.NoError(err)
	next, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
	must.NoError(err)
	checks.Equal(int64(1), next.MeetingsAdded)
}

func TestMeetingIdentitySurvivesMovesAndSeparateSources(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		kind := hegel.Draw(ht, hegel.Integers(0, 3))
		st, source, imp := broadFixture(t)
		first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
		require.NoError(ht, err)
		var id int64
		require.NoError(ht, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
		meeting := hydrationMeeting()
		parents := []Parent{{Type: "page_id", PageID: "moved"}, {Type: "data_source_id", DataSourceID: "moved"}, {Type: "block_id", BlockID: "moved"}, {Type: "agent_id", AgentID: "moved"}}
		meeting.Parent = parents[kind]
		meeting.LastEditedTime = "2026-08-29T11:00:00Z"
		installMeetingHeader(t, source.fakeHydrationSource, meeting)
		source.query.Results = nil
		updated := paragraph("transcript-1", "MOVE_SCOPED_TRANSCRIPT", false)
		source.blocks[updated.ID] = &updated
		second, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
		require.NoError(ht, err)
		assert.Equal(ht, int64(1), second.MeetingsUpdated)
		raw, err := st.GetMessageRaw(id)
		require.NoError(ht, err)
		assert.Contains(ht, string(raw), "MOVE_SCOPED_TRANSCRIPT")
		assert.Contains(ht, string(raw), `"`+meeting.Parent.Type+`"`)
		third, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", TokenType: "pat"})
		require.NoError(ht, err)
		assert.Zero(ht, third.MeetingsAdded)
		assert.Zero(ht, third.MeetingsUpdated)
		sameRaw, err := st.GetMessageRaw(id)
		require.NoError(ht, err)
		assert.Equal(ht, raw, sameRaw)
		_, err = st.GetOrCreateSource(SourceType, "other")
		require.NoError(ht, err)
		source.query.Results = []MeetingNote{meeting}
		other, err := imp.Import(t.Context(), ImportOptions{Identifier: "other", TokenType: "pat"})
		require.NoError(ht, err)
		assert.NotEqual(ht, first.SourceID, other.SourceID)
		assert.Equal(ht, int64(1), other.MeetingsAdded)
		var count int
		require.NoError(ht, st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&count))
		assert.Equal(ht, 2, count)
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
		require.NoError(ht, err)
		for _, delta := range []int{-1, 0, 1} {
			limits := defaultDiscoveryLimits()
			limits.Bytes = len(encoded) + delta
			err := s.validateTransientWith(limits)
			if delta < 0 {
				require.Error(ht, err)
			} else {
				require.NoError(ht, err)
			}
		}
	}, hegel.WithTestCases(64), hegel.WithDatabase(""))
}
