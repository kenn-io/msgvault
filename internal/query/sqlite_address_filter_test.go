package query

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func explainSearchPlan(t *testing.T, env *testEnv, q *search.Query) string {
	t.Helper()
	conditions, args, ftsJoin := env.Engine.buildSearchQueryParts(env.Ctx, q)
	sqlText := searchResultsSQL(ftsJoin, strings.Join(conditions, " AND "))
	rows, err := env.DB.QueryContext(env.Ctx, "EXPLAIN QUERY PLAN "+sqlText, append(args, 50, 0)...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		plan.WriteString(detail + "\n")
	}
	require.NoError(t, rows.Err())
	return plan.String()
}

// A rare address must look up matching messages by rowid instead of walking
// every message newest-first and probing its recipients.
func TestSearchAddressFiltersDriveFromParticipants(t *testing.T) {
	env := newTestEnv(t)
	for _, q := range []*search.Query{
		{FromAddrs: []string{"@example.com"}},
		{FromAddrs: []string{"alice@example.com"}},
		{ToAddrs: []string{"@company.org"}},
		{CcAddrs: []string{"dan@other.net"}},
		{BccAddrs: []string{"@example.com"}},
	} {
		plan := explainSearchPlan(t, env, q)
		assert.Contains(t, plan, "SEARCH m USING INTEGER PRIMARY KEY", "%+v\n%s", q, plan)
		assert.NotContains(t, plan, "SCAN m USING INDEX idx_messages_sent_at", "%+v\n%s", q, plan)
	}
}

func TestSearchFromDomainTreatsWildcardsLiterally(t *testing.T) {
	env := newTestEnv(t)
	id := env.AddParticipant(dbtest.ParticipantOpts{Email: new("x@myxco.example"), Domain: "myxco.example"})
	env.AddMessage(dbtest.MessageOpts{Subject: "wildcard probe", FromID: id})

	assert.Empty(t, env.MustSearch(&search.Query{FromAddrs: []string{"@my_co.example"}}, 50, 0),
		"'_' must not act as a LIKE wildcard")
	assert.Empty(t, env.MustSearch(&search.Query{FromAddrs: []string{"@my%co.example"}}, 50, 0),
		"'%' must not act as a LIKE wildcard")
	assert.Len(t, env.MustSearch(&search.Query{FromAddrs: []string{"@myxco.example"}}, 50, 0), 1)
}

// A domain with many matches still returns stable newest-first pages.
func TestSearchFromDomainManyMatchesPagesNewestFirst(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	sender := env.AddParticipant(dbtest.ParticipantOpts{Email: new("bulk@bulk.example"), Domain: "bulk.example"})
	for i := range 30 {
		env.AddMessage(dbtest.MessageOpts{
			Subject: fmt.Sprintf("bulk %02d", i),
			SentAt:  fmt.Sprintf("2024-06-%02d 10:00:00", i+1),
			FromID:  sender,
		})
	}
	q := &search.Query{FromAddrs: []string{"@bulk.example"}}
	first := env.MustSearch(q, 10, 0)
	second := env.MustSearch(q, 10, 10)
	require.Len(first, 10)
	require.Len(second, 10)
	assert.Equal("bulk 29", first[0].Subject)
	assert.Equal("bulk 19", second[0].Subject)
	for i := 1; i < len(first); i++ {
		assert.False(first[i].SentAt.After(first[i-1].SentAt), "newest first")
	}
}
