package store_test

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// skipUnlessPostgres skips the calling test unless MSGVAULT_TEST_DB points at
// PostgreSQL. The tsvector 1MB limit and its body cap are PostgreSQL-only —
// SQLite's FTS5 imposes no such bound, so these tests have nothing to exercise
// on SQLite.
func skipUnlessPostgres(t *testing.T) {
	t.Helper()
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(testDB, "postgres://") && !strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("PG-only: tsvector body cap; requires MSGVAULT_TEST_DB pointing at PostgreSQL")
	}
}

// oversizedBody builds a body of distinct tokens whose total length exceeds the
// PostgreSQL tsvector cap. Each token is unique so to_tsvector would, without a
// cap, produce a tsvector large enough to trip the 1MB "string is too long for
// tsvector" error. ~1.5MB of "tokNNNNNNN " words yields well over 100k lexemes.
func oversizedBody() string {
	var b strings.Builder
	b.Grow(1_600_000)
	i := 0
	for b.Len() < 1_500_000 {
		b.WriteString("tok")
		b.WriteString(itoaPad(i))
		b.WriteByte(' ')
		i++
	}
	return b.String()
}

// itoaPad renders n as a zero-padded 7-digit string so every token is the same
// width and reliably distinct.
func itoaPad(n int) string {
	const digits = "0123456789"
	buf := []byte("0000000")
	for i := len(buf) - 1; i >= 0 && n > 0; i-- {
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf)
}

// multibyteOversizedBody builds a body of MANY DISTINCT 2-byte multibyte tokens
// whose to_tsvector (before any truncation) exceeds PostgreSQL's 1MB tsvector
// limit — the adversarial shape that a character cap CANNOT bound (finding B1).
// Each token is a unique sequence of Cyrillic 2-byte runes, so every token is a
// distinct lexeme; the verification reviewer reproduced SQLSTATE 54000 at
// ~1.23MB with ~600000 such 2-byte chars. We emit ~700000 distinct 2-byte runes
// of token content (~1.4MB raw), comfortably past the limit pre-truncation.
//
// Distinctness: there are ~1900 single 2-byte runes in U+0410..U+044F-style
// ranges, not enough for hundreds of thousands of distinct single-rune tokens,
// so each token is a fixed-width run of several 2-byte runes encoding a counter
// in a base-N digit alphabet of 2-byte runes. That guarantees uniqueness while
// keeping every character a 2-byte rune.
func multibyteOversizedBody() string {
	// A 16-rune alphabet of distinct 2-byte Cyrillic runes (U+0410..U+041F).
	alphabet := []rune("АБВГДЕЖЗИЙКЛМНОП")
	const runesPerToken = 5 // 16^5 ≈ 1M distinct tokens — plenty
	const targetRunes = 700_000

	var b strings.Builder
	b.Grow(targetRunes*2 + targetRunes) // 2 bytes/rune + spaces
	emitted := 0
	for i := 0; emitted < targetRunes; i++ {
		n := i
		for range runesPerToken {
			b.WriteRune(alphabet[n%len(alphabet)])
			n /= len(alphabet)
			emitted++
		}
		b.WriteByte(' ')
	}
	return b.String()
}

func nullSearchFTSCount(t *testing.T, st interface{ DB() *sql.DB }) int {
	t.Helper()
	var n int
	requirepkg.NoError(t, st.DB().QueryRow(
		"SELECT COUNT(*) FROM messages WHERE search_fts IS NULL").Scan(&n),
		"count NULL search_fts")
	return n
}

// TestPG_FTSUpsert_OversizedBodyTruncates (finding T2a) verifies that indexing
// a single message whose body exceeds the tsvector cap SUCCEEDS on PostgreSQL —
// the body is truncated (LEFT cap) rather than the UPDATE erroring — and leaves
// search_fts non-NULL. Without the cap this would fail with SQLSTATE 54000
// ("string is too long for tsvector") and leave the row permanently NULL.
func TestPG_FTSUpsert_OversizedBodyTruncates(t *testing.T) {
	require := requirepkg.New(t)
	skipUnlessPostgres(t)
	f := storetest.New(t)
	require.True(f.Store.FTS5Available(), "FTS must be available on PG")

	msgID := f.CreateMessage("oversized-upsert")

	// UpsertFTS routes through PostgreSQLDialect.FTSUpsert, where the LEFT cap
	// lives. A >1MB body must not error.
	err := f.Store.UpsertFTS(msgID, "subject line", oversizedBody(),
		"alice@example.com", "bob@example.com", "")
	require.NoError(err, "UpsertFTS with oversized body must succeed (truncated indexing)")

	var isNull bool
	require.NoError(f.Store.DB().QueryRow(
		"SELECT search_fts IS NULL FROM messages WHERE id = $1", msgID).Scan(&isNull),
		"probe search_fts")
	assertpkg.False(t, isNull, "search_fts must be non-NULL after truncated upsert")
}

// TestPG_BackfillFTS_OversizedBodyDoesNotWedge (finding T2b) seeds >5000
// messages with one oversized body in a middle batch, then runs BackfillFTS.
// It must complete WITHOUT error and every row — including those AFTER the
// oversized one — must end with non-NULL search_fts. The LEFT cap makes the
// oversized row index fine; the row-by-row retry fallback is the belt-and-
// suspenders guarantee that no single bad row can wedge later batches.
func TestPG_BackfillFTS_OversizedBodyDoesNotWedge(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	skipUnlessPostgres(t)
	f := storetest.New(t)
	require.True(f.Store.FTS5Available(), "FTS must be available on PG")

	// 5500 messages spans at least two 5000-row backfill batches. The oversized
	// body lands at index 2500 — inside the FIRST batch — so we can assert that
	// every row in LATER batches still gets indexed.
	const total = 5500
	const oversizedIdx = 2500
	ids := f.CreateMessages(total)
	require.Len(ids, total)

	require.NoError(f.Store.UpsertMessageBody(ids[oversizedIdx],
		sql.NullString{String: oversizedBody(), Valid: true}, sql.NullString{}),
		"attach oversized body")

	// Give a couple of normal rows after the oversized one bodies too, so the
	// "rows after the bad one are indexed" claim is concrete.
	require.NoError(f.Store.UpsertMessageBody(ids[total-1],
		sql.NullString{String: "final sentinel body apricot", Valid: true}, sql.NullString{}),
		"attach sentinel body")

	n, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS must complete despite an oversized body")
	assert.Equal(int64(total), n, "every message should be indexed")

	// No row may be left NULL — not the oversized one, nor any after it.
	assert.Equal(0, nullSearchFTSCount(t, f.Store),
		"no message may be left with NULL search_fts after backfill")

	// The sentinel row (last id, after the oversized one) is searchable.
	_, sentinelTotal, err := f.Store.SearchMessages("apricot", 0, 10)
	require.NoError(err, "SearchMessages apricot")
	assert.Equal(int64(1), sentinelTotal, "sentinel row after oversized must be searchable")
}

// TestPG_FTSUpsert_MultibyteOversizedBody (finding B1) is the regression for
// the case the OLD comment falsely claimed could not happen: a body of MANY
// DISTINCT 2-byte multibyte tokens whose tsvector exceeds 1MB before any cap.
// The character-only LEFT cap cannot bound such input, so without the Go
// byte-truncation in FTSUpsert this would raise SQLSTATE 54000.
//
// The fix byte-truncates the body (rune-safe) to maxFTSBodyBytes BEFORE
// binding, so the sync UpsertFTS path must NOT return a hard error and the row
// must end up indexed (search_fts non-NULL). The key property: a pathological
// multibyte body can never wedge the sync FTS path.
func TestPG_FTSUpsert_MultibyteOversizedBody(t *testing.T) {
	require := requirepkg.New(t)
	skipUnlessPostgres(t)
	f := storetest.New(t)
	require.True(f.Store.FTS5Available(), "FTS must be available on PG")

	msgID := f.CreateMessage("multibyte-upsert")
	body := multibyteOversizedBody()
	// Sanity: the raw body really is large enough to overflow tsvector
	// pre-truncation (so the test would catch a regression that removed the
	// byte cap), and is genuinely multibyte.
	require.Greater(len(body), 1_100_000, "raw multibyte body must exceed 1MB of bytes")
	require.Less(len([]rune(body)), len(body), "body must contain multibyte runes")

	err := f.Store.UpsertFTS(msgID, "subject line", body,
		"alice@example.com", "bob@example.com", "")
	require.NoError(err,
		"UpsertFTS with a multibyte oversized body must NOT return a hard error (byte-truncated)")

	var isNull bool
	require.NoError(f.Store.DB().QueryRow(
		"SELECT search_fts IS NULL FROM messages WHERE id = $1", msgID).Scan(&isNull),
		"probe search_fts")
	assertpkg.False(t, isNull,
		"search_fts must be non-NULL after byte-truncated multibyte upsert")
}

// TestPG_FTSUpsert_OversizedSubjectAndRecipients (finding C2/D1) verifies that
// the byte-truncation now applies to EVERY tsvector input field, not just the
// body. The inputs here are MULTIBYTE oversized documents (distinct 2-byte
// tokens, >1MB tsvector pre-cap) — the adversarial shape the SQL LEFT char cap
// CANNOT bound. A message whose SUBJECT and recipient (to) lists each exceed
// the tsvector cap must still UpsertFTS-succeed (no hard SQLSTATE 54000 to the
// caller) and end up indexed (search_fts non-NULL). Before the D1 fix
// subject/from/to/cc got only the SQL LEFT char cap, so a multibyte oversized
// subject alone could still trip 54000 and leave the row NULL; this test fails
// without the per-field Go byte-truncation in FTSUpsert.
func TestPG_FTSUpsert_OversizedSubjectAndRecipients(t *testing.T) {
	skipUnlessPostgres(t)
	f := storetest.New(t)
	requirepkg.True(t, f.Store.FTS5Available(), "FTS must be available on PG")

	// Multibyte oversized inputs: the char-only LEFT cap cannot bound these, so
	// reaching a non-NULL search_fts proves the D1 per-field byte-truncation ran.
	bigSubject := multibyteOversizedBody() // distinct 2-byte tokens, >1MB pre-cap
	bigTo := multibyteOversizedBody()      // oversized multibyte recipient list
	requirepkg.Greater(t, len(bigSubject), 1_100_000, "multibyte subject must exceed 1MB of bytes")
	requirepkg.Less(t, len([]rune(bigSubject)), len(bigSubject), "subject must contain multibyte runes")

	t.Run("oversized subject only", func(t *testing.T) {
		msgID := f.CreateMessage("oversized-subject")
		err := f.Store.UpsertFTS(msgID, bigSubject, "normal body apricot",
			"alice@example.com", "bob@example.com", "")
		requirepkg.NoError(t, err,
			"UpsertFTS with a multibyte oversized subject must succeed (subject is now byte-truncated)")

		var isNull bool
		requirepkg.NoError(t, f.Store.DB().QueryRow(
			"SELECT search_fts IS NULL FROM messages WHERE id = $1", msgID).Scan(&isNull),
			"probe search_fts")
		assertpkg.False(t, isNull, "search_fts must be non-NULL after byte-truncated subject upsert")
	})

	t.Run("oversized subject and recipient list", func(t *testing.T) {
		msgID := f.CreateMessage("oversized-subject-and-to")
		err := f.Store.UpsertFTS(msgID, bigSubject, "normal body banana",
			"alice@example.com", bigTo, "")
		requirepkg.NoError(t, err,
			"UpsertFTS with multibyte oversized subject + recipient list must succeed (all fields byte-truncated)")

		var isNull bool
		requirepkg.NoError(t, f.Store.DB().QueryRow(
			"SELECT search_fts IS NULL FROM messages WHERE id = $1", msgID).Scan(&isNull),
			"probe search_fts")
		assertpkg.False(t, isNull, "search_fts must be non-NULL after byte-truncated subject+recipients upsert")
	})
}

// TestPG_BackfillFTS_MultibyteOversizedBodyDoesNotWedge (finding B1) seeds a
// batch containing one message whose DB body is a pathological multibyte
// document (distinct 2-byte tokens, >1MB tsvector pre-cap). The backfill SQL
// path applies only the LEFT char cap (a UTF-8-safe byte truncation is not
// cleanly available in SQL), so for adversarial multibyte input the row may
// still overflow — in which case backfillFTSRowByRow skips it. The pinned
// property: BackfillFTS COMPLETES WITHOUT ERROR and indexes every OTHER row,
// regardless of whether the pathological row itself survives the char cap.
func TestPG_BackfillFTS_MultibyteOversizedBodyDoesNotWedge(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	skipUnlessPostgres(t)
	f := storetest.New(t)
	require.True(f.Store.FTS5Available(), "FTS must be available on PG")

	const total = 6
	const oversizedIdx = 3 // middle row, so rows after it must still index
	ids := f.CreateMessages(total)
	require.Len(ids, total)

	// Give every row a distinct searchable token so per-row indexing is
	// provable; overwrite the middle row with the pathological multibyte body.
	tokens := []string{
		"alphafruit", "betafruit", "gammafruit",
		"deltafruit", "epsilonfruit", "zetafruit",
	}
	for i, id := range ids {
		require.NoError(f.Store.UpsertMessageBody(id,
			sql.NullString{String: tokens[i] + " shared", Valid: true}, sql.NullString{}),
			"attach body %d", i)
	}
	require.NoError(f.Store.UpsertMessageBody(ids[oversizedIdx],
		sql.NullString{String: multibyteOversizedBody(), Valid: true}, sql.NullString{}),
		"attach multibyte oversized body")

	n, err := f.Store.BackfillFTS(nil)
	require.NoError(err,
		"BackfillFTS must COMPLETE without error despite a pathological multibyte body")

	// Every row EXCEPT possibly the pathological one is indexed; at most one row
	// (the oversized multibyte body, if it still overflows the char cap) is
	// skipped. The invariant is: BackfillFTS never errors and never wedges later
	// rows.
	nulls := nullSearchFTSCount(t, f.Store)
	assert.LessOrEqual(nulls, 1,
		"at most the one pathological row may be left NULL; got %d", nulls)
	assert.GreaterOrEqual(n, int64(total-1),
		"every row except (at most) the pathological one must be indexed")

	// Every non-oversized token — including the ones AFTER the pathological row
	// — must be searchable, proving the bad row never wedged later rows.
	for i, tok := range tokens {
		if i == oversizedIdx {
			continue
		}
		_, hits, searchErr := f.Store.SearchMessages(tok, 0, 10)
		require.NoError(searchErr, "SearchMessages %q", tok)
		assert.Equal(int64(1), hits, "token %q must be searchable after backfill", tok)
	}
}
