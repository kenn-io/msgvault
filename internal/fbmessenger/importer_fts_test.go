//go:build fts5

package fbmessenger

import (
	"strings"
	"testing"

	"github.com/wesm/msgvault/internal/testutil"
)

// TestImportDYI_MojibakeFTSIndexed verifies that mojibake-repaired body
// text (e.g. "café") lands in message_bodies AND is indexed by FTS5 so a
// direct MATCH query returns a hit. Gated on the fts5 build tag so the
// FTS assertion is always active under the project's canonical
// `go test -tags fts5 ./...` invocation.
func TestImportDYI_MojibakeFTSIndexed(t *testing.T) {
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_simple")
	if !st.FTS5Available() {
		t.Fatal("FTS5 build tag set but FTS5 not available in this binary")
	}

	// The body stored in message_bodies must contain literal "café".
	var body string
	if err := st.DB().QueryRow(
		`SELECT body_text FROM message_bodies WHERE body_text LIKE '%café%'`,
	).Scan(&body); err != nil {
		t.Fatalf("body query: %v", err)
	}
	if !strings.Contains(body, "café") {
		t.Errorf("body=%q", body)
	}

	var count int
	if err := st.DB().QueryRow(
		"SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?", "café",
	).Scan(&count); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if count < 1 {
		t.Errorf("fts match for café: got %d want >=1", count)
	}
}

// TestImportDYI_ReactionsDualPath verifies that reactions land both as
// first-class rows in the reactions table and as an appended
// "[reacted: ...]" suffix in body_text that FTS5 can match. Gated on
// the fts5 build tag; the FTS MATCH assertion is unconditional.
func TestImportDYI_ReactionsDualPath(t *testing.T) {
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_simple")
	if !st.FTS5Available() {
		t.Fatal("FTS5 build tag set but FTS5 not available in this binary")
	}

	// Count reactions on the message that contains café.
	var n int
	if err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM reactions r
		JOIN message_bodies b ON b.message_id = r.message_id
		WHERE b.body_text LIKE '%café%'
	`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("reactions=%d want 2", n)
	}

	// Body text must contain the appended [reacted: ...] summary.
	var bodyCount int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM message_bodies WHERE body_text LIKE '%[reacted:%'`,
	).Scan(&bodyCount); err != nil {
		t.Fatal(err)
	}
	if bodyCount < 1 {
		t.Errorf("body with [reacted: suffix: got %d want >=1", bodyCount)
	}

	if err := st.DB().QueryRow(
		"SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?", "reacted",
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Errorf("fts match reacted: %d want >=1", n)
	}
}
