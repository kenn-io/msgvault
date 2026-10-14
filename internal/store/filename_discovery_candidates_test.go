package store_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/filenamediscovery"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func filenameCandidateFixture(t *testing.T, names []string) (*storetest.Fixture, []int64) {
	t.Helper()
	f := storetest.New(t)
	message := f.CreateMessage("filename-candidates")
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	var ids []int64
	for _, name := range names {
		var id int64
		require.NoError(t, tx.QueryRow(f.Store.Rebind(`INSERT INTO attachments
			(message_id, filename, storage_path) VALUES(?,?, '') RETURNING id`), message, name).Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, tx.Commit())
	filenameReady(t, f.Store)
	return f, ids
}

func filenameCandidates(t *testing.T, s *store.Store, raw string) store.FilenameCandidateSet {
	t.Helper()
	q, err := filenamediscovery.ParseQuery(raw)
	require.NoError(t, err)
	ctx, release, err := s.BeginReadSnapshotContext(t.Context())
	require.NoError(t, err)
	defer release()
	set, err := s.FilenameCandidatesContext(ctx, q, store.FilenameSuggestionScope{})
	require.NoError(t, err)
	assert.LessOrEqual(t, set.PostingRows, 2048)
	assert.LessOrEqual(t, len(set.IDs), 500)
	return set
}

// Duplicate posting rows still consume the work budget. A short seed omits
// typo streams, so these literal populations have exactly 2047/2048/2049 rows.
func TestFilenameCandidatesPostingBudget(t *testing.T) {
	for _, total := range []int{2047, 2048, 2049} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			names := make([]string, 499)
			for i := range names {
				names[i] = "repa repb repc repd"
				if i < total-1996 {
					names[i] += " repe"
				}
				names[i] += ".pdf"
			}
			f, ids := filenameCandidateFixture(t, names)
			f.Store.DB().SetMaxOpenConns(1)
			set := filenameCandidates(t, f.Store, "rep")
			assert.Equal(t, min(total, 2048), set.PostingRows)
			assert.ElementsMatch(t, ids, set.IDs, "posting duplicates never enlarge the authority set")
			assert.Equal(t, total >= 2048, set.CandidateLimitReached,
				"an exact-cap full terminal page may conservatively remain incomplete")
		})
	}
}

func TestFilenameCandidatesDistinctBudget(t *testing.T) {
	for _, total := range []int{499, 500, 501} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			assert := assert.New(t)

			names := make([]string, total)
			for i := range names {
				names[i] = "rep.pdf"
			}
			f, ids := filenameCandidateFixture(t, names)
			set := filenameCandidates(t, f.Store, "rep.pdf")
			newest := slices.Clone(ids[max(total-500, 0):])
			slices.Reverse(newest)
			assert.Equal(newest, set.IDs, "equal-rank candidates are newest first")
			assert.Equal(total > 500, set.CandidateLimitReached)
			assert.Equal(2*total, set.PostingRows,
				"full-name and seed-exact duplicates are charged, including after the candidate cap")
		})
	}
}

func TestFilenameCandidatesDuplicateTypoStreams(t *testing.T) {
	names := make([]string, 499)
	for i := range names {
		names[i] = "report.pdf"
	}
	f, ids := filenameCandidateFixture(t, names)
	set := filenameCandidates(t, f.Store, "report.pdf")
	assert.ElementsMatch(t, ids, set.IDs)
	assert.Equal(t, 2048, set.PostingRows)
	assert.True(t, set.CandidateLimitReached, "deletion-posting duplicates cannot be free or imply exhausted ranges")
}
