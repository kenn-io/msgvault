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
	return dbtest.ExplainQueryPlan(t, env.DB, sqlText, append(args, 50, 0)...)
}

// Choose the FTS result page before looking up sender and conversation details.
func TestSearchFTSSelectsPageBeforeDetails(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.EnableFTS()
	plan := explainSearchPlan(t, env, search.Parse("invoice"))
	sort := strings.Index(plan, "USE TEMP B-TREE FOR ORDER BY")
	conversation := strings.Index(plan, "SEARCH conv USING INTEGER PRIMARY KEY")
	require.GreaterOrEqual(t, sort, 0, plan)
	require.GreaterOrEqual(t, conversation, 0, plan)
	assert.Less(t, sort, conversation, plan)
}

func TestSearchFTSPagingPreservesDetails(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	sender := env.AddParticipant(dbtest.ParticipantOpts{Email: new("billing@example.com"), DisplayName: new("Billing")})
	_, err := env.DB.Exec(`UPDATE conversations SET title = 'Invoice thread' WHERE id = 1`)
	require.NoError(err)
	env.AddMessage(dbtest.MessageOpts{Subject: "invoice one", FromID: sender})
	second := env.AddMessage(dbtest.MessageOpts{Subject: "invoice two"})
	_, err = env.DB.Exec(`UPDATE messages SET sender_id = ? WHERE id = ?`, sender, second)
	require.NoError(err)
	env.AddMessage(dbtest.MessageOpts{Subject: "invoice three", FromID: sender, SentAt: "2024-06-01 10:00:00"})
	env.EnableFTS()
	q := search.Parse("invoice")
	full := env.MustSearch(q, 10, 0)
	require.Len(full, 3)
	var paged []MessageSummary
	for offset := 0; offset < 4; offset += 2 {
		paged = append(paged, env.MustSearch(q, 2, offset)...)
	}
	assert.Equal(full, paged)
	assert.Empty(env.MustSearch(q, 2, 4))
	for _, msg := range paged {
		assert.Equal("billing@example.com", msg.FromEmail)
		assert.Equal("Billing", msg.FromName)
		assert.Equal("Invoice thread", msg.ConversationTitle)
	}
}

// A rare address must look up matching messages by rowid instead of walking
// every message newest-first and probing its recipients.
func TestSearchAddressFiltersDriveFromParticipants(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	for _, q := range []*search.Query{
		{FromAddrs: []string{"@example.com"}},
		{FromAddrs: []string{"alice@example.com"}},
		{ToAddrs: []string{"@company.org"}},
		{CcAddrs: []string{"dan@other.net"}},
		{BccAddrs: []string{"@example.com"}},
	} {
		plan := explainSearchPlan(t, env, q)
		assert.NotRegexp(t, `(?m)^SCAN m(?: |$)`, plan, "%+v\n%s", q, plan)
	}
}

func TestSearchFromDomainTreatsWildcardsLiterally(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
