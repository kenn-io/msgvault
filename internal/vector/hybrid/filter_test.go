package hybrid

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
)

func newFilterTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	requirepkg.NoError(t, err, "open")
	t.Cleanup(func() { _ = db.Close() })

	schema := `
CREATE TABLE participants (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    email_address TEXT
);
CREATE TABLE labels (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT
);
`
	_, err = db.Exec(schema)
	requirepkg.NoError(t, err, "schema")
	participants := []string{
		"alice@example.com",
		"bob@example.com",
		"carol@other.com",
		"dave.work@example.com",
	}
	for _, p := range participants {
		_, err := db.Exec(`INSERT INTO participants (email_address) VALUES (?)`, p)
		requirepkg.NoError(t, err, "insert participant")
	}
	for _, l := range []string{"INBOX", "Work", "Archive"} {
		_, err := db.Exec(`INSERT INTO labels (name) VALUES (?)`, l)
		requirepkg.NoError(t, err, "insert label")
	}
	return db
}

func sortedIDs(ids []int64) []int64 {
	out := append([]int64(nil), ids...)
	slices.Sort(out)
	return out
}

// TestBuildFilter_AddressesResolveViaSubstring confirms that
// from:/to:/cc:/bcc: use the same substring-LIKE semantic as the
// existing SQLite search path, so vector/hybrid and FTS agree on which
// participants match a token.
func TestBuildFilter_AddressesResolveViaSubstring(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`from:example.com to:alice cc:other.com`)
	f, err := BuildFilter(ctx, db, q)
	require.NoError(err, "BuildFilter")

	// from:example.com → alice, bob, dave.work (all @example.com).
	// Single from: token, so SenderGroups has one group with the
	// substring-match set.
	require.Len(f.SenderGroups, 1, "SenderGroups should have one group")
	assert.Lenf(f.SenderGroups[0], 3, "want 3 ids (all @example.com); got %v", f.SenderGroups)
	// to:alice → one group with one id.
	require.Len(f.ToGroups, 1, "ToGroups should have one group")
	assert.Lenf(f.ToGroups[0], 1, "want one group with one id (alice); got %v", f.ToGroups)
	// cc:other.com → one group with one id (carol).
	require.Len(f.CcGroups, 1, "CcGroups should have one group")
	assert.Lenf(f.CcGroups[0], 1, "want one group with one id (carol); got %v", f.CcGroups)
}

// TestBuildFilter_SizeAndSubjectAndDate confirms that larger:/smaller:,
// subject:, and date bounds flow through to the Filter struct unchanged.
func TestBuildFilter_SizeAndSubjectAndDate(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`larger:1M smaller:10M subject:quarterly subject:"offsite plan" after:2025-01-01 before:2025-06-01`)
	f, err := BuildFilter(ctx, db, q)
	require.NoError(err, "BuildFilter")
	if assert.NotNil(f.LargerThan, "LargerThan") {
		assert.Equal(int64(1024*1024), *f.LargerThan)
	}
	if assert.NotNil(f.SmallerThan, "SmallerThan") {
		assert.Equal(int64(10*1024*1024), *f.SmallerThan)
	}
	require.Len(f.SubjectSubstrings, 2)
	assert.Equal("quarterly", f.SubjectSubstrings[0])
	assert.Equal("offsite plan", f.SubjectSubstrings[1])
	assert.NotNil(f.After, "After should be parsed")
	assert.NotNil(f.Before, "Before should be parsed")
}

// TestBuildFilter_LabelsAndAttachments checks the label: and
// has:attachment operators.
func TestBuildFilter_LabelsAndAttachments(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`label:Work has:attachment`)
	f, err := BuildFilter(ctx, db, q)
	require.NoError(err, "BuildFilter")
	require.Len(f.LabelGroups, 1, "LabelGroups should have one group")
	assert.Lenf(f.LabelGroups[0], 1, "want one group with one id (Work); got %v", f.LabelGroups)
	if assert.NotNil(f.HasAttachment, "HasAttachment") {
		assert.True(*f.HasAttachment)
	}
}

// TestBuildFilter_EmptyQueryYieldsEmptyFilter covers the
// "no operators" path.
func TestBuildFilter_EmptyQueryYieldsEmptyFilter(t *testing.T) {
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`lunch plans`)
	f, err := BuildFilter(ctx, db, q)
	requirepkg.NoError(t, err, "BuildFilter")
	assertpkg.Truef(t, f.IsEmpty(), "filter not empty: %+v", f)
}

// TestBuildFilter_NonexistentSenderReturnsSentinel guards the
// "operator was present but matched zero rows" path. Without a
// sentinel, an unknown from: address would resolve to an empty
// SenderGroups slice, which the backend treats as "no filter" —
// broadening the search instead of returning zero hits.
func TestBuildFilter_NonexistentSenderReturnsSentinel(t *testing.T) {
	require := requirepkg.New(t)
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`from:nobody@nowhere.invalid`)
	f, err := BuildFilter(ctx, db, q)
	require.NoError(err, "BuildFilter")
	require.Lenf(f.SenderGroups, 1, "want one group with sentinel; got %v", f.SenderGroups)
	require.Len(f.SenderGroups[0], 1)
	assertpkg.Negative(t, f.SenderGroups[0][0], "want negative sentinel id")
}

// TestBuildFilter_NonexistentLabelReturnsSentinel: same as above but
// for labels. Critical because the label path used to do exact-name
// lookups with IN (...), and an unknown label silently became a no-op.
func TestBuildFilter_NonexistentLabelReturnsSentinel(t *testing.T) {
	require := requirepkg.New(t)
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`label:nonexistent-label-xyz`)
	f, err := BuildFilter(ctx, db, q)
	require.NoError(err, "BuildFilter")
	require.Lenf(f.LabelGroups, 1, "want one group with sentinel; got %v", f.LabelGroups)
	require.Len(f.LabelGroups[0], 1)
	assertpkg.Negative(t, f.LabelGroups[0][0], "want negative sentinel id")
}

// TestBuildFilter_RepeatedSenderTokens_PerTokenGroups asserts that
// repeated `from:` operators each become their own group. The backend
// AND-combines groups at the message level — `from:alice from:bob`
// requires the message to have a `from` participant matching alice
// AND a `from` participant matching bob. A message with two `from`
// recipients (one alice, one bob) satisfies both tokens; a message
// with only one `from` participant cannot. This mirrors the existing
// SQLite search path (internal/store/api.go), which emits one EXISTS
// per `from:` token at the message level.
func TestBuildFilter_RepeatedSenderTokens_PerTokenGroups(t *testing.T) {
	ctx := context.Background()
	db := newFilterTestDB(t)

	t.Run("two real tokens become two non-sentinel groups", func(t *testing.T) {
		require := requirepkg.New(t)
		assert := assertpkg.New(t)
		q := search.Parse(`from:alice from:bob`)
		f, err := BuildFilter(ctx, db, q)
		require.NoError(err, "BuildFilter")
		require.Lenf(f.SenderGroups, 2, "want 2 groups (one per from: token); got %v", f.SenderGroups)
		for i, g := range f.SenderGroups {
			assert.Lenf(g, 1, "SenderGroups[%d]=%v, want exactly one positive id", i, g)
			if len(g) == 1 {
				assert.GreaterOrEqualf(g[0], int64(0), "SenderGroups[%d]=%v, want positive id", i, g)
			}
		}
	})

	t.Run("one missing token sentinels only that group", func(t *testing.T) {
		require := requirepkg.New(t)
		assert := assertpkg.New(t)
		q := search.Parse(`from:alice from:nobody@nowhere.invalid`)
		f, err := BuildFilter(ctx, db, q)
		require.NoError(err, "BuildFilter")
		require.Lenf(f.SenderGroups, 2, "want 2 groups; got %v", f.SenderGroups)
		require.Len(f.SenderGroups[0], 1)
		assert.GreaterOrEqualf(f.SenderGroups[0][0], int64(0), "alice resolved")
		require.Len(f.SenderGroups[1], 1)
		assert.Negativef(f.SenderGroups[1][0], "want sentinel (nobody resolves empty)")
	})

	t.Run("substring tokens collect all matching participants per group", func(t *testing.T) {
		require := requirepkg.New(t)
		assert := assertpkg.New(t)
		// from:example.com → alice, bob, dave.work all match @example.com.
		// from:work → only dave.work. Two groups, IDs preserved per group.
		q := search.Parse(`from:example.com from:work`)
		f, err := BuildFilter(ctx, db, q)
		require.NoError(err, "BuildFilter")
		require.Lenf(f.SenderGroups, 2, "want 2 groups; got %v", f.SenderGroups)
		assert.Lenf(f.SenderGroups[0], 3,
			"@example.com matches alice/bob/dave.work; got %v", sortedIDs(f.SenderGroups[0]))
		assert.Lenf(f.SenderGroups[1], 1,
			"only dave.work has 'work' substring; got %v", f.SenderGroups[1])
	})
}

// TestBuildFilter_RepeatedRecipientTokens_PerTokenGroups asserts that
// repeated to:/cc:/bcc: operators each become their own group. The
// backend AND-combines groups (OR within), so `to:alice to:bob`
// requires the message to have a `to` recipient matching alice AND a
// `to` recipient matching bob — preserving the SQLite path's per-token
// AND semantics for multi-valued recipient fields.
func TestBuildFilter_RepeatedRecipientTokens_PerTokenGroups(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`to:alice to:bob`)
	f, err := BuildFilter(ctx, db, q)
	require.NoError(err, "BuildFilter")
	require.Lenf(f.ToGroups, 2, "want 2 groups (one per to: token); got %v", f.ToGroups)
	for i, g := range f.ToGroups {
		assert.Lenf(g, 1, "ToGroups[%d]=%v, want exactly 1 id (alice/bob each match one participant)", i, g)
		if len(g) == 1 {
			assert.GreaterOrEqualf(g[0], int64(0), "ToGroups[%d]=%v should not contain negative sentinel", i, g)
		}
	}
}

// TestBuildFilter_RepeatedRecipientTokens_OneEmptySentinelsThatGroup
// confirms that when one of several recipient tokens resolves to zero
// participants, only that group gets the sentinel — the other groups
// keep their real IDs. The backend's AND-of-groups means the sentinel
// group still poisons the entire field (its EXISTS clause cannot
// match), so the message set narrows to zero — same effect as the FTS
// path, but without conflating the two tokens at resolution time.
func TestBuildFilter_RepeatedRecipientTokens_OneEmptySentinelsThatGroup(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`to:alice to:nobody@nowhere.invalid`)
	f, err := BuildFilter(ctx, db, q)
	require.NoError(err, "BuildFilter")
	require.Lenf(f.ToGroups, 2, "want 2 groups; got %v", f.ToGroups)
	require.Len(f.ToGroups[0], 1)
	assert.GreaterOrEqualf(f.ToGroups[0][0], int64(0), "alice resolved")
	require.Len(f.ToGroups[1], 1)
	assert.Negativef(f.ToGroups[1][0], "want sentinel (nobody resolves empty)")
}

// TestBuildFilter_RepeatedLabelTokens_PerTokenGroups is the label-side
// counterpart of TestBuildFilter_RepeatedRecipientTokens_PerTokenGroups.
func TestBuildFilter_RepeatedLabelTokens_PerTokenGroups(t *testing.T) {
	ctx := context.Background()
	db := newFilterTestDB(t)

	t.Run("two real labels become two groups", func(t *testing.T) {
		require := requirepkg.New(t)
		assert := assertpkg.New(t)
		q := search.Parse(`label:Work label:Archive`)
		f, err := BuildFilter(ctx, db, q)
		require.NoError(err, "BuildFilter")
		require.Lenf(f.LabelGroups, 2, "want 2 groups; got %v", f.LabelGroups)
		for i, g := range f.LabelGroups {
			assert.Lenf(g, 1, "LabelGroups[%d]=%v, want one positive id", i, g)
			if len(g) == 1 {
				assert.GreaterOrEqualf(g[0], int64(0), "LabelGroups[%d]=%v, want positive id", i, g)
			}
		}
	})

	t.Run("one missing token sentinels only that group", func(t *testing.T) {
		require := requirepkg.New(t)
		assert := assertpkg.New(t)
		q := search.Parse(`label:Work label:nonexistent-xyz`)
		f, err := BuildFilter(ctx, db, q)
		require.NoError(err, "BuildFilter")
		require.Lenf(f.LabelGroups, 2, "want 2 groups; got %v", f.LabelGroups)
		require.Len(f.LabelGroups[0], 1)
		assert.GreaterOrEqualf(f.LabelGroups[0][0], int64(0), "Work resolved")
		require.Len(f.LabelGroups[1], 1)
		assert.Negativef(f.LabelGroups[1][0], "want sentinel")
	})
}

// TestBuildFilter_LabelsMatchCaseInsensitiveSubstring verifies the
// label resolution matches the SQLite path's semantics: a substring
// of the configured label name, case-folded. Previously
// resolveLabelIDs did exact-name IN (...) matching, which failed on
// the common `label:work` / stored-label `Work` mismatch and on
// partial names.
func TestBuildFilter_LabelsMatchCaseInsensitiveSubstring(t *testing.T) {
	ctx := context.Background()
	db := newFilterTestDB(t)

	cases := []struct {
		name       string
		query      string
		wantGroups int
		wantInOnly int // expected length of the single group
	}{
		{"lowercased exact", `label:work`, 1, 1}, // "Work"
		{"partial prefix", `label:arch`, 1, 1},   // "Archive"
		{"partial uppercase", `label:INB`, 1, 1}, // "INBOX"
		{"no match", `label:nowhere`, 1, 1},      // sentinel (no real matches)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := search.Parse(c.query)
			f, err := BuildFilter(ctx, db, q)
			requirepkg.NoError(t, err, "BuildFilter")
			requirepkg.Lenf(t, f.LabelGroups, c.wantGroups,
				"query %q: LabelGroups %v", c.query, f.LabelGroups)
			assertpkg.Lenf(t, f.LabelGroups[0], c.wantInOnly,
				"query %q: LabelGroups[0] %v", c.query, f.LabelGroups[0])
		})
	}
}
