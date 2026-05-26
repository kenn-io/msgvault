package store_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func currentBackend() string {
	db := os.Getenv("MSGVAULT_TEST_DB")
	if strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://") {
		return "postgres"
	}
	return "sqlite"
}

// TestFTSRankParityFixture seeds a fixed corpus and runs a handful of
// representative queries, printing the resulting message-ID order with
// t.Log. Run it under SQLite and under PostgreSQL (MSGVAULT_TEST_DB=...)
// and diff the captured logs: identical top-N ordering is the parity
// claim. The test asserts only the invariants that must hold on both
// backends (subject hit ranks above body hit), so it does not fail on
// the residual scorer-math differences documented in PG_STATUS.md.
func TestFTSRankParityFixture(t *testing.T) {
	st := testutil.NewTestStore(t)

	src, err := st.GetOrCreateSource("gmail", "fixture@example.com")
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "fixture-thread", "Fixture")
	require.NoError(t, err, "EnsureConversation")

	mk := func(label, subject, body, fromAddr string) int64 {
		id, err := st.UpsertMessage(&store.Message{
			ConversationID:  convID,
			SourceID:        src.ID,
			SourceMessageID: label,
			MessageType:     "email",
			Subject:         nullString(subject),
			Snippet:         nullString(body),
			SizeEstimate:    100,
		})
		require.NoError(t, err, "UpsertMessage(%q)", label)
		require.NoError(t, st.UpsertFTS(id, subject, body, fromAddr, "", ""), "UpsertFTS(%q)", label)
		return id
	}

	// Each row is { sourceMessageID, subject, body, fromAddr }.
	// Tokens chosen so a query for any token below appears in known
	// columns of known rows.
	rows := []struct {
		label, subject, body, from string
	}{
		{"r1", "invoice march filler",
			"alpha beta gamma delta epsilon",
			"billing acme noreply@example.com"},
		{"r2", "weekly newsletter filler filler",
			"invoice mentioned in body once filler",
			"news editor@example.com"},
		{"r3", "team standup notes filler",
			"march meeting prep filler filler",
			"invoice billing@example.com"},
		{"r4", "march madness alpha filler",
			"invoice invoice invoice filler filler",
			"sender alpha@example.com"},
		{"r5", "alpha alpha alpha filler",
			"delta filler filler filler filler",
			"editor news@example.com"},
		{"r6", "delta filler filler filler",
			"alpha alpha filler filler filler",
			"noreply billing@example.com"},
	}
	ids := make(map[string]int64, len(rows))
	for _, r := range rows {
		ids[r.label] = mk(r.label, r.subject, r.body, r.from)
	}

	// Reverse map for human-readable logging.
	labelOf := make(map[int64]string, len(ids))
	for label, id := range ids {
		labelOf[id] = label
	}

	queries := []string{
		"invoice", // appears in subject(r1), body(r2,r4), from(r3)
		"march",   // subject(r4), body(r3), subject(r1)
		"alpha",   // subject(r4,r5), body(r6,r1), from(r4)
		"delta",   // subject(r6), body(r5,r1)
		"billing", // from(r3,r6), from(r1)
	}

	backend := currentBackend()

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			results, total, err := st.SearchMessages(q, 0, 20)
			require.NoError(t, err, "SearchMessages(%q)", q)
			order := make([]string, 0, len(results))
			for _, r := range results {
				order = append(order, labelOf[r.ID])
			}
			t.Logf("backend=%s query=%q total=%d order=%s",
				backend, q, total, strings.Join(order, ","))

			// Sanity: rows where the token appears only in subject
			// must rank above rows where it appears only in body.
			// This is the invariant both backends must honor.
			invariants := invariantsFor(q)
			for _, inv := range invariants {
				higher, lower := inv[0], inv[1]
				hPos := indexOfLabel(order, higher)
				lPos := indexOfLabel(order, lower)
				if hPos == -1 || lPos == -1 {
					continue // not both present, skip
				}
				assert.Less(t, hPos, lPos,
					"query %q: row %q (pos %d) should rank above row %q (pos %d) — full order: %v",
					q, higher, hPos, lower, lPos, order)
			}
		})
	}
}

// invariantsFor returns (higher, lower) pairs that must hold for a query.
// Built from the fixture in TestFTSRankParityFixture: when one row has
// the term only in subject and another only in body, subject wins.
func invariantsFor(q string) [][2]string {
	switch q {
	case "invoice":
		// r1 has invoice in subject only; r2 has it in body only.
		return [][2]string{{"r1", "r2"}}
	case "march":
		// r1 and r4 have march in subject; r3 has it only in body.
		return [][2]string{{"r1", "r3"}, {"r4", "r3"}}
	case "delta":
		// r6 has delta in subject only; r5 has it in body only.
		return [][2]string{{"r6", "r5"}}
	}
	return nil
}

func indexOfLabel(order []string, label string) int {
	for i, l := range order {
		if l == label {
			return i
		}
	}
	return -1
}
