package store_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/filenamediscovery"
	"go.kenn.io/msgvault/internal/personscope"
	personresolver "go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type syntheticFilename struct {
	id     int64
	name   string
	person bool
}
type filenameScaleFixture struct {
	st                           *store.Store
	sourceID, personID           int64
	files                        []syntheticFilename
	bootstrap, incremental, idle time.Duration
}
type filenameScaleCase struct {
	name, query            string
	empty, person, limited bool
	limit                  int
}

func filenameScaleCases() []filenameScaleCase {
	return []filenameScaleCase{
		{name: "exact", query: "record-000000000001.pdf", limit: 20},
		{name: "typo", query: "recrod-000000000001.pdf", limit: 20},
		{name: "short-prefix", query: "re", limited: true, limit: 20},
		{name: "no-hit", query: "absent-111111111111.pdf", limit: 20},
		// Intentional transposition for collision recall.
		{name: "dense-collision", query: "rec" + "rod", limited: true, limit: 20},
		// Scope filters apply during admission, so the 1k recall fixture can
		// prove a scoped population complete, including an empty one.
		{name: "restrictive-empty", query: "record", empty: true, limit: 20},
		{name: "maximum-name", query: strings.Repeat("m", 64), limit: 20},
		{name: "64-person", query: "record", person: true, limit: 100},
	}
}

// Setup uses the real schema and attachment journal. Names are arithmetic,
// with no random seed or private archive. The extra attachment has exactly
// 512 normalized runes; its 64-rune query stays within request limits.
func newFilenameScaleFixture(ctx context.Context, tb testing.TB, st *store.Store, size int) *filenameScaleFixture {
	tb.Helper()
	source, err := st.GetOrCreateSource("gmail", "scale@example.test")
	require.NoError(tb, err)
	conv, err := st.EnsureConversation(source.ID, "scale-thread", "Synthetic scale")
	require.NoError(tb, err)
	members := make([]int64, 64)
	for i := range members {
		members[i], err = st.EnsureParticipant(fmt.Sprintf("member-%02d@example.test", i), fmt.Sprintf("Scale Member %02d", i), "example.test")
		require.NoError(tb, err)
	}
	person, _, err := st.CreatePersonFromParticipant(members[0])
	require.NoError(tb, err)
	tx, err := st.DB().BeginTx(ctx, nil)
	require.NoError(tb, err)
	defer func() { _ = tx.Rollback() }()
	for _, pid := range members[1:] {
		_, err = tx.ExecContext(ctx, st.Rebind(`INSERT INTO person_participants(person_id,participant_id) VALUES(?,?)`), person.ID, pid)
		require.NoError(tb, err)
	}
	message, err := tx.PrepareContext(ctx, st.Rebind(`INSERT INTO messages(source_id,source_message_id,conversation_id,message_type,sent_at) VALUES(?,?,?,'email',?) RETURNING id`))
	require.NoError(tb, err)
	defer func() { _ = message.Close() }()
	attachment, err := tx.PrepareContext(ctx, st.Rebind(`INSERT INTO attachments(message_id,filename,mime_type,storage_path) VALUES(?,?,'application/pdf','') RETURNING id`))
	require.NoError(tb, err)
	defer func() { _ = attachment.Close() }()
	recipient, err := tx.PrepareContext(ctx, st.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,display_name) VALUES(?,?,'to','Synthetic member')`))
	require.NoError(tb, err)
	defer func() { _ = recipient.Close() }()
	f := &filenameScaleFixture{st: st, sourceID: source.ID, personID: person.ID, files: make([]syntheticFilename, 0, size+1)}
	// Spread the person's files across attachment IDs so neither retrieval
	// order favors them.
	stride := max(size/len(members), 1)
	for i := range size + 1 {
		name := fmt.Sprintf("record-%012d.pdf", i+1)
		if i == size {
			name = strings.Repeat(strings.Repeat("m", 64)+" ", 7) + strings.Repeat("m", 57)
		}
		var mid, aid int64
		require.NoError(tb, message.QueryRowContext(ctx, source.ID, fmt.Sprintf("scale-%d", i), conv, time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)).Scan(&mid))
		require.NoError(tb, attachment.QueryRowContext(ctx, mid, name).Scan(&aid))
		bound := i < size && i%stride == 0 && i/stride < len(members)
		if bound {
			_, err = recipient.ExecContext(ctx, mid, members[i/stride])
			require.NoError(tb, err)
		}
		f.files = append(f.files, syntheticFilename{id: aid, name: name, person: bound})
	}
	require.NoError(tb, tx.Commit())
	started := time.Now()
	drainFilenameScale(ctx, tb, st, size)
	f.bootstrap = time.Since(started)
	// Measure one actual journaled rename and catchup, then restore the fixture.
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE attachments SET filename='updated-000000000001.pdf' WHERE id=?`), f.files[0].id)
	require.NoError(tb, err)
	started = time.Now()
	drainFilenameScale(ctx, tb, st, size)
	f.incremental = time.Since(started)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE attachments SET filename=? WHERE id=?`), f.files[0].name, f.files[0].id)
	require.NoError(tb, err)
	drainFilenameScale(ctx, tb, st, size)
	started = time.Now()
	idle, err := st.FilenameDiscoveryBatchContext(ctx)
	require.NoError(tb, err)
	require.False(tb, idle.More)
	require.Zero(tb, idle.Processed)
	f.idle = time.Since(started)
	return f
}

func drainFilenameScale(ctx context.Context, tb testing.TB, st *store.Store, size int) {
	tb.Helper()
	// Bound transitions by fixture population, not wall-clock throughput.
	for range 20 + size/100 {
		batch, err := st.FilenameDiscoveryBatchContext(ctx)
		require.NoError(tb, err)
		require.LessOrEqual(tb, batch.Processed, 500)
		if batch.Status.Phase == "ready" && !batch.More {
			return
		}
	}
	require.FailNow(tb, "synthetic filename projection did not reach finite readiness")
}

// filenameScalePostingBudget is the documented physical posting budget:
// 2,048 rows unscoped and 8,192 rows when a scope filter rejects postings.
func filenameScalePostingBudget(tc filenameScaleCase) int {
	if tc.empty || tc.person {
		return 8192
	}
	return 2048
}

type filenameScaleMatch struct {
	file syntheticFilename
	rank filenamediscovery.Rank
}

func (f *filenameScaleFixture) exhaustive(q filenamediscovery.Query, tc filenameScaleCase) []filenameScaleMatch {
	matches := make([]filenameScaleMatch, 0)
	for _, file := range f.files {
		if tc.empty || tc.person && !file.person {
			continue
		}
		name, admitted := filenamediscovery.ProjectName(file.name)
		if !admitted {
			continue
		}
		rank, matched := filenamediscovery.Match(q, name)
		if matched {
			matches = append(matches, filenameScaleMatch{file: file, rank: rank})
		}
	}
	slices.SortFunc(matches, func(a, b filenameScaleMatch) int {
		if order := cmp.Compare(a.rank, b.rank); order != 0 {
			return order
		}
		return cmp.Compare(b.file.id, a.file.id)
	})
	return matches
}

type filenameScalePersonAdapter struct{ st *store.Store }

func (a filenameScalePersonAdapter) GetPersonContext(ctx context.Context, id int64) (*store.Person, error) {
	return a.st.FilenamePersonContext(ctx, id)
}
func (f *filenameScaleFixture) evidence(ctx context.Context, tb testing.TB, q filenamediscovery.Query, tc filenameScaleCase) (store.FilenameCandidateSet, int, []int64) {
	tb.Helper()
	ctx, release, err := f.st.BeginReadSnapshotContext(ctx)
	require.NoError(tb, err)
	defer release()
	scope := store.FilenameSuggestionScope{}
	if tc.empty {
		scope.SourceIDs = []int64{f.sourceID + 999999}
	}
	if tc.person {
		resolved, err := personresolver.Resolve(ctx, filenameScalePersonAdapter{f.st}, personresolver.Reference{Kind: personresolver.ReferencePerson, ID: f.personID}, []personscope.Direction{personscope.ToPerson})
		require.NoError(tb, err)
		require.Len(tb, resolved.Scope.ParticipantIDs, 64)
		scope.Person = &resolved.Scope
	}
	candidates, err := f.st.FilenameCandidatesContext(ctx, q, scope)
	require.NoError(tb, err)
	rows, err := f.st.FilenameSuggestionRowsContext(ctx, candidates.IDs, scope)
	require.NoError(tb, err)
	accepted := make([]int64, 0, len(rows))
	for _, row := range rows {
		name, ok := filenamediscovery.ProjectName(row.File.Filename)
		if !ok {
			continue
		}
		if _, ok := filenamediscovery.Match(q, name); ok {
			accepted = append(accepted, row.File.ID)
		}
	}
	return candidates, len(rows), accepted
}

func (f *filenameScaleFixture) request(ctx context.Context, tb testing.TB, router http.Handler, tc filenameScaleCase) ([]int64, bool) {
	tb.Helper()
	request := api.FileSuggestionHTTPRequest{FilenameQuery: tc.query, Limit: &tc.limit}
	if tc.empty {
		request.SourceIDs = []int64{f.sourceID + 999999}
	}
	path := "/api/v1/files/suggest"
	var body any = request
	if tc.person {
		path = fmt.Sprintf("/api/v1/people/%d/files/suggest", f.personID)
		body = api.PersonFileSuggestionHTTPRequest{FileSuggestionHTTPRequest: request, Directions: []personscope.Direction{personscope.ToPerson}}
	}
	raw, err := json.Marshal(body)
	require.NoError(tb, err)
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	require.Equal(tb, http.StatusOK, w.Code, w.Body.String())
	var response struct {
		Suggestions []struct {
			ID int64 `json:"id"`
		} `json:"suggestions"`
		Limited  bool   `json:"candidate_limit_reached"`
		Revision string `json:"index_revision"`
	}
	require.NoError(tb, json.Unmarshal(w.Body.Bytes(), &response))
	require.NotEmpty(tb, response.Revision)
	ids := make([]int64, len(response.Suggestions))
	for i, row := range response.Suggestions {
		ids[i] = row.ID
	}
	return ids, response.Limited
}

func filenameScaleRouter(st *store.Store) http.Handler {
	return api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}).Router()
}
func scaleMatchIDs(matches []filenameScaleMatch, limit int) []int64 {
	ids := make([]int64, min(limit, len(matches)))
	for i := range ids {
		ids[i] = matches[i].file.id
	}
	return ids
}
func filenameScaleOverlap(want, got []int64) int {
	set := make(map[int64]bool, len(got))
	for _, id := range got {
		set[id] = true
	}
	count := 0
	for _, id := range want {
		if set[id] {
			count++
		}
	}
	return count
}

func TestFilenameDiscoverySyntheticRecall(t *testing.T) {
	st := testutil.NewTestStore(t)
	f := newFilenameScaleFixture(t.Context(), t, st, 1000)
	for _, tc := range filenameScaleCases() {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)

			q, err := filenamediscovery.ParseQuery(tc.query)
			require.NoError(t, err)
			candidates, verified, _ := f.evidence(t.Context(), t, q, tc)
			got, limited := f.request(t.Context(), t, filenameScaleRouter(st), tc)
			assert.LessOrEqual(candidates.PostingRows, filenameScalePostingBudget(tc))
			assert.LessOrEqual(len(candidates.IDs), 500)
			assert.LessOrEqual(verified, 500)
			assert.LessOrEqual(len(got), 100)
			assert.Equal(tc.limited, limited)
			assert.Equal(candidates.CandidateLimitReached, limited)
			want := scaleMatchIDs(f.exhaustive(q, tc), tc.limit)
			if !limited {
				assert.Equal(want, got, "complete indexed results equal exhaustive policy results")
			}
			switch tc.name {
			case "exact", "typo":
				assert.Equal([]int64{f.files[0].id}, got)
			case "maximum-name":
				assert.Equal([]int64{f.files[len(f.files)-1].id}, got)
			case "no-hit", "restrictive-empty":
				assert.Empty(got)
			case "64-person":
				assert.Len(got, 64)
			}
		})
	}
}

// BenchmarkFilenameDiscovery compares the real HTTP path with a test-only
// exhaustive oracle under identical fixtures and matching policy. The normal
// durable SQLite opener is intentional: maintenance costs include commits.
// sqlite_dbstat is a measurement-only native-driver tag for physical page bytes.
func BenchmarkFilenameDiscovery(b *testing.B) {
	for _, size := range []int{1000, 10000, 100000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			st, err := store.Open(filepath.Join(b.TempDir(), "synthetic.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, st.Close()) })
			require.NoError(b, st.InitSchema())
			f := newFilenameScaleFixture(b.Context(), b, st, size)
			var indexBytes int64
			require.NoError(b, st.DB().QueryRowContext(b.Context(), `SELECT COALESCE(SUM(pgsize),0) FROM dbstat WHERE name IN (SELECT name FROM sqlite_schema WHERE tbl_name LIKE 'filename_discovery_%')`).Scan(&indexBytes), "physical index measurement requires -tags 'fts5 sqlite_vec sqlite_dbstat'")
			require.Positive(b, indexBytes)
			for _, tc := range filenameScaleCases() {
				b.Run(tc.name, func(b *testing.B) {
					q, err := filenamediscovery.ParseQuery(tc.query)
					require.NoError(b, err)
					candidates, verified, accepted := f.evidence(b.Context(), b, q, tc)
					oracle := f.exhaustive(q, tc)
					want := scaleMatchIDs(oracle, tc.limit)
					router := filenameScaleRouter(st)
					got, limited := f.request(b.Context(), b, router, tc)
					// Scoped completeness depends on fixture size; the limited
					// metric reports it, and complete results must match the oracle.
					require.LessOrEqual(b, candidates.PostingRows, filenameScalePostingBudget(tc))
					require.LessOrEqual(b, len(candidates.IDs), 500)
					require.LessOrEqual(b, verified, 500)
					require.LessOrEqual(b, len(got), 100)
					if !limited {
						require.Equal(b, want, got)
					}
					overlap := filenameScaleOverlap(want, got)
					recall := 1.0
					if len(want) > 0 {
						recall = float64(overlap) / float64(len(want))
					}
					allIDs := scaleMatchIDs(oracle, len(oracle))
					missingCandidates := len(allIDs) - filenameScaleOverlap(allIDs, accepted)
					b.Run("indexed", func(b *testing.B) {
						b.ReportAllocs()
						for b.Loop() {
							_, _ = f.request(b.Context(), b, router, tc)
						}
						b.ReportMetric(float64(candidates.PostingRows), "postings/op")
						b.ReportMetric(float64(len(candidates.IDs)), "admitted/op")
						b.ReportMetric(float64(verified), "verified/op")
						b.ReportMetric(float64(len(got)), "returned/op")
						b.ReportMetric(recall, "recall")
						b.ReportMetric(float64(len(want)-overlap), "missing-output/op")
						b.ReportMetric(float64(missingCandidates), "missing-pool/op")
						flag := 0.0
						if limited {
							flag = 1
						}
						b.ReportMetric(flag, "limited")
						b.ReportMetric(float64(indexBytes), "index_B")
						b.ReportMetric(float64(f.bootstrap.Nanoseconds()), "bootstrap_ns")
						b.ReportMetric(float64(f.incremental.Nanoseconds()), "incremental_ns")
						b.ReportMetric(float64(f.idle.Nanoseconds()), "idle_ns")
					})
					b.Run("exhaustive", func(b *testing.B) {
						b.ReportAllocs()
						for b.Loop() {
							_ = scaleMatchIDs(f.exhaustive(q, tc), tc.limit)
						}
					})
				})
			}
		})
	}
}
