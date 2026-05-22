package hybrid

import (
	"context"
	"database/sql"
	"sort"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"go.kenn.io/msgvault/internal/search"
)

func newFilterTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
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
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	participants := []string{
		"alice@example.com",
		"bob@example.com",
		"carol@other.com",
		"dave.work@example.com",
	}
	for _, p := range participants {
		if _, err := db.Exec(`INSERT INTO participants (email_address) VALUES (?)`, p); err != nil {
			t.Fatalf("insert participant: %v", err)
		}
	}
	for _, l := range []string{"INBOX", "Work", "Archive"} {
		if _, err := db.Exec(`INSERT INTO labels (name) VALUES (?)`, l); err != nil {
			t.Fatalf("insert label: %v", err)
		}
	}
	return db
}

func sortedIDs(ids []int64) []int64 {
	out := append([]int64(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TestBuildFilter_AddressesResolveViaSubstring confirms that
// from:/to:/cc:/bcc: use the same substring-LIKE semantic as the
// existing SQLite search path, so vector/hybrid and FTS agree on which
// participants match a token.
func TestBuildFilter_AddressesResolveViaSubstring(t *testing.T) {
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`from:example.com to:alice cc:other.com`)
	f, err := BuildFilter(ctx, db, q)
	if err != nil {
		t.Fatalf("BuildFilter: %v", err)
	}

	// from:example.com → alice, bob, dave.work (all @example.com).
	// Single from: token, so SenderGroups has one group with the
	// substring-match set.
	if len(f.SenderGroups) != 1 || len(f.SenderGroups[0]) != 3 {
		t.Errorf("SenderGroups = %v, want one group with 3 ids (all @example.com)", f.SenderGroups)
	}
	// to:alice → one group with one id.
	if len(f.ToGroups) != 1 || len(f.ToGroups[0]) != 1 {
		t.Errorf("ToGroups = %v, want one group with one id (alice)", f.ToGroups)
	}
	// cc:other.com → one group with one id (carol).
	if len(f.CcGroups) != 1 || len(f.CcGroups[0]) != 1 {
		t.Errorf("CcGroups = %v, want one group with one id (carol)", f.CcGroups)
	}
}

// TestBuildFilter_SizeAndSubjectAndDate confirms that larger:/smaller:,
// subject:, and date bounds flow through to the Filter struct unchanged.
func TestBuildFilter_SizeAndSubjectAndDate(t *testing.T) {
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`larger:1M smaller:10M subject:quarterly subject:"offsite plan" after:2025-01-01 before:2025-06-01`)
	f, err := BuildFilter(ctx, db, q)
	if err != nil {
		t.Fatalf("BuildFilter: %v", err)
	}
	if f.LargerThan == nil || *f.LargerThan != 1024*1024 {
		t.Errorf("LargerThan = %v, want 1048576", f.LargerThan)
	}
	if f.SmallerThan == nil || *f.SmallerThan != 10*1024*1024 {
		t.Errorf("SmallerThan = %v, want 10485760", f.SmallerThan)
	}
	if len(f.SubjectSubstrings) != 2 ||
		f.SubjectSubstrings[0] != "quarterly" ||
		f.SubjectSubstrings[1] != "offsite plan" {
		t.Errorf("SubjectSubstrings = %v, want [quarterly offsite plan]", f.SubjectSubstrings)
	}
	if f.After == nil {
		t.Error("After = nil, want parsed")
	}
	if f.Before == nil {
		t.Error("Before = nil, want parsed")
	}
}

// TestBuildFilter_LabelsAndAttachments checks the label: and
// has:attachment operators.
func TestBuildFilter_LabelsAndAttachments(t *testing.T) {
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`label:Work has:attachment`)
	f, err := BuildFilter(ctx, db, q)
	if err != nil {
		t.Fatalf("BuildFilter: %v", err)
	}
	if len(f.LabelGroups) != 1 || len(f.LabelGroups[0]) != 1 {
		t.Errorf("LabelGroups = %v, want one group with one id (Work)", f.LabelGroups)
	}
	if f.HasAttachment == nil || !*f.HasAttachment {
		t.Errorf("HasAttachment = %v, want true", f.HasAttachment)
	}
}

// TestBuildFilter_EmptyQueryYieldsEmptyFilter covers the
// "no operators" path.
func TestBuildFilter_EmptyQueryYieldsEmptyFilter(t *testing.T) {
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`lunch plans`)
	f, err := BuildFilter(ctx, db, q)
	if err != nil {
		t.Fatalf("BuildFilter: %v", err)
	}
	if !f.IsEmpty() {
		t.Errorf("filter not empty: %+v", f)
	}
}

// TestBuildFilter_NonexistentSenderReturnsSentinel guards the
// "operator was present but matched zero rows" path. Without a
// sentinel, an unknown from: address would resolve to an empty
// SenderGroups slice, which the backend treats as "no filter" —
// broadening the search instead of returning zero hits.
func TestBuildFilter_NonexistentSenderReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`from:nobody@nowhere.invalid`)
	f, err := BuildFilter(ctx, db, q)
	if err != nil {
		t.Fatalf("BuildFilter: %v", err)
	}
	if len(f.SenderGroups) != 1 || len(f.SenderGroups[0]) != 1 || f.SenderGroups[0][0] >= 0 {
		t.Errorf("SenderGroups=%v, want one group with the negative sentinel id", f.SenderGroups)
	}
}

// TestBuildFilter_NonexistentLabelReturnsSentinel: same as above but
// for labels. Critical because the label path used to do exact-name
// lookups with IN (...), and an unknown label silently became a no-op.
func TestBuildFilter_NonexistentLabelReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`label:nonexistent-label-xyz`)
	f, err := BuildFilter(ctx, db, q)
	if err != nil {
		t.Fatalf("BuildFilter: %v", err)
	}
	if len(f.LabelGroups) != 1 || len(f.LabelGroups[0]) != 1 || f.LabelGroups[0][0] >= 0 {
		t.Errorf("LabelGroups=%v, want one group with the negative sentinel id", f.LabelGroups)
	}
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
		q := search.Parse(`from:alice from:bob`)
		f, err := BuildFilter(ctx, db, q)
		if err != nil {
			t.Fatalf("BuildFilter: %v", err)
		}
		if len(f.SenderGroups) != 2 {
			t.Fatalf("SenderGroups=%v, want 2 groups (one per from: token)", f.SenderGroups)
		}
		for i, g := range f.SenderGroups {
			if len(g) != 1 || g[0] < 0 {
				t.Errorf("SenderGroups[%d]=%v, want exactly one positive id", i, g)
			}
		}
	})

	t.Run("one missing token sentinels only that group", func(t *testing.T) {
		q := search.Parse(`from:alice from:nobody@nowhere.invalid`)
		f, err := BuildFilter(ctx, db, q)
		if err != nil {
			t.Fatalf("BuildFilter: %v", err)
		}
		if len(f.SenderGroups) != 2 {
			t.Fatalf("SenderGroups=%v, want 2 groups", f.SenderGroups)
		}
		if len(f.SenderGroups[0]) != 1 || f.SenderGroups[0][0] < 0 {
			t.Errorf("SenderGroups[0]=%v, want positive id (alice resolved)", f.SenderGroups[0])
		}
		if len(f.SenderGroups[1]) != 1 || f.SenderGroups[1][0] >= 0 {
			t.Errorf("SenderGroups[1]=%v, want sentinel (nobody resolves empty)", f.SenderGroups[1])
		}
	})

	t.Run("substring tokens collect all matching participants per group", func(t *testing.T) {
		// from:example.com → alice, bob, dave.work all match @example.com.
		// from:work → only dave.work. Two groups, IDs preserved per group.
		q := search.Parse(`from:example.com from:work`)
		f, err := BuildFilter(ctx, db, q)
		if err != nil {
			t.Fatalf("BuildFilter: %v", err)
		}
		if len(f.SenderGroups) != 2 {
			t.Fatalf("SenderGroups=%v, want 2 groups", f.SenderGroups)
		}
		if len(f.SenderGroups[0]) != 3 {
			t.Errorf("SenderGroups[0]=%v, want 3 ids (@example.com matches alice/bob/dave.work)", sortedIDs(f.SenderGroups[0]))
		}
		if len(f.SenderGroups[1]) != 1 {
			t.Errorf("SenderGroups[1]=%v, want 1 id (only dave.work has 'work' substring)", f.SenderGroups[1])
		}
	})
}

// TestBuildFilter_RepeatedRecipientTokens_PerTokenGroups asserts that
// repeated to:/cc:/bcc: operators each become their own group. The
// backend AND-combines groups (OR within), so `to:alice to:bob`
// requires the message to have a `to` recipient matching alice AND a
// `to` recipient matching bob — preserving the SQLite path's per-token
// AND semantics for multi-valued recipient fields.
func TestBuildFilter_RepeatedRecipientTokens_PerTokenGroups(t *testing.T) {
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`to:alice to:bob`)
	f, err := BuildFilter(ctx, db, q)
	if err != nil {
		t.Fatalf("BuildFilter: %v", err)
	}
	if len(f.ToGroups) != 2 {
		t.Fatalf("ToGroups=%v, want 2 groups (one per to: token)", f.ToGroups)
	}
	for i, g := range f.ToGroups {
		if len(g) != 1 {
			t.Errorf("ToGroups[%d]=%v, want exactly 1 id (alice/bob each match one participant)", i, g)
		}
		if g[0] < 0 {
			t.Errorf("ToGroups[%d]=%v contains negative sentinel; both tokens resolved", i, g)
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
	ctx := context.Background()
	db := newFilterTestDB(t)

	q := search.Parse(`to:alice to:nobody@nowhere.invalid`)
	f, err := BuildFilter(ctx, db, q)
	if err != nil {
		t.Fatalf("BuildFilter: %v", err)
	}
	if len(f.ToGroups) != 2 {
		t.Fatalf("ToGroups=%v, want 2 groups", f.ToGroups)
	}
	if len(f.ToGroups[0]) != 1 || f.ToGroups[0][0] < 0 {
		t.Errorf("ToGroups[0]=%v, want one positive id (alice resolved)", f.ToGroups[0])
	}
	if len(f.ToGroups[1]) != 1 || f.ToGroups[1][0] >= 0 {
		t.Errorf("ToGroups[1]=%v, want sentinel (nobody resolves empty)", f.ToGroups[1])
	}
}

// TestBuildFilter_RepeatedLabelTokens_PerTokenGroups is the label-side
// counterpart of TestBuildFilter_RepeatedRecipientTokens_PerTokenGroups.
func TestBuildFilter_RepeatedLabelTokens_PerTokenGroups(t *testing.T) {
	ctx := context.Background()
	db := newFilterTestDB(t)

	t.Run("two real labels become two groups", func(t *testing.T) {
		q := search.Parse(`label:Work label:Archive`)
		f, err := BuildFilter(ctx, db, q)
		if err != nil {
			t.Fatalf("BuildFilter: %v", err)
		}
		if len(f.LabelGroups) != 2 {
			t.Fatalf("LabelGroups=%v, want 2 groups", f.LabelGroups)
		}
		for i, g := range f.LabelGroups {
			if len(g) != 1 || g[0] < 0 {
				t.Errorf("LabelGroups[%d]=%v, want one positive id", i, g)
			}
		}
	})

	t.Run("one missing token sentinels only that group", func(t *testing.T) {
		q := search.Parse(`label:Work label:nonexistent-xyz`)
		f, err := BuildFilter(ctx, db, q)
		if err != nil {
			t.Fatalf("BuildFilter: %v", err)
		}
		if len(f.LabelGroups) != 2 {
			t.Fatalf("LabelGroups=%v, want 2 groups", f.LabelGroups)
		}
		if len(f.LabelGroups[0]) != 1 || f.LabelGroups[0][0] < 0 {
			t.Errorf("LabelGroups[0]=%v, want positive id (Work resolved)", f.LabelGroups[0])
		}
		if len(f.LabelGroups[1]) != 1 || f.LabelGroups[1][0] >= 0 {
			t.Errorf("LabelGroups[1]=%v, want sentinel", f.LabelGroups[1])
		}
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
		c := c
		t.Run(c.name, func(t *testing.T) {
			q := search.Parse(c.query)
			f, err := BuildFilter(ctx, db, q)
			if err != nil {
				t.Fatalf("BuildFilter: %v", err)
			}
			if len(f.LabelGroups) != c.wantGroups {
				t.Errorf("query %q: len(LabelGroups)=%d, want %d (got %v)",
					c.query, len(f.LabelGroups), c.wantGroups, f.LabelGroups)
				return
			}
			if len(f.LabelGroups[0]) != c.wantInOnly {
				t.Errorf("query %q: len(LabelGroups[0])=%d, want %d (got %v)",
					c.query, len(f.LabelGroups[0]), c.wantInOnly, f.LabelGroups[0])
			}
		})
	}
}
