package query

import (
	"fmt"
	"strconv"
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
	_, err := env.DB.ExecContext(t.Context(), `UPDATE conversations SET title = 'Invoice thread' WHERE id = 1`)
	require.NoError(err)
	env.AddMessage(dbtest.MessageOpts{Subject: "invoice one", FromID: sender})
	second := env.AddMessage(dbtest.MessageOpts{Subject: "invoice two"})
	_, err = env.DB.ExecContext(t.Context(), `UPDATE messages SET sender_id = ? WHERE id = ?`, sender, second)
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

func TestSearchCommonWordExcludesCutoffTies(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	_, err := env.DB.ExecContext(t.Context(), `WITH RECURSIVE seq(x) AS (
		VALUES(1) UNION ALL SELECT x+1 FROM seq WHERE x < 3014
	) INSERT INTO messages (id, source_message_id, source_id, conversation_id, message_type, sent_at, subject)
	SELECT 100+x, 'window-' || x, 1, 1, 'email',
		CASE WHEN x <= 3000 THEN '2025-01-01 00:00:00' ELSE '2025-02-01 00:00:00' END,
		'invoice' FROM seq`)
	require.NoError(t, err)
	env.EnableFTS()
	q := search.Parse("invoice")
	conditions, args, ftsJoin := env.Engine.buildSearchQueryParts(env.Ctx, q)
	for _, tc := range []struct {
		offset int
		rows   int
		full   bool
	}{
		{offset: 0, rows: 7, full: true},
		{offset: 7, rows: 7, full: true},
		{offset: 14, rows: 0, full: false},
	} {
		t.Run(strconv.Itoa(tc.offset), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			page, full, err := env.Engine.searchNewestFirst(env.Ctx, q, conditions, args, ftsJoin, 7, tc.offset)
			require.NoError(err)
			assert.Len(page, tc.rows, "the cutoff date's tie group must stay outside the window")
			assert.Equal(tc.full, full)
		})
	}
}

// The newest-first window for common words must keep room policy: a Cold
// room's matches inside the window are not part of the page.
func TestSearchCommonWordWindowExcludesColdRooms(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	for _, stmt := range []string{
		`INSERT INTO sources (id, source_type, identifier) VALUES (90, 'slack', 'T0WINDOW')`,
		`INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES (90, 90, 'cold-room', 'group_chat', 'Cold room')`,
		`INSERT INTO conversation_policy (conversation_id, effective_tier) VALUES (90, 'cold')`,
		// Alternate Cold-room and Full-room matches through the newest window.
		`WITH RECURSIVE seq(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM seq WHERE x < 2000)
		INSERT INTO messages (id, source_message_id, source_id, conversation_id, message_type, sent_at, subject)
		SELECT 5000+x, 'mixed-' || x,
			CASE WHEN x % 2 = 0 THEN 90 ELSE 1 END,
			CASE WHEN x % 2 = 0 THEN 90 ELSE 1 END,
			CASE WHEN x % 2 = 0 THEN 'slack' ELSE 'email' END,
			datetime('2025-03-01 00:00:00', '-' || x || ' minutes'), 'invoice' FROM seq`,
	} {
		_, err := env.DB.ExecContext(t.Context(), stmt)
		require.NoError(err)
	}
	env.EnableFTS()
	q := search.Parse("invoice")
	conditions, args, ftsJoin := env.Engine.buildSearchQueryParts(env.Ctx, q)
	page, full, err := env.Engine.searchNewestFirst(env.Ctx, q, conditions, args, ftsJoin, 7, 0)
	require.NoError(err)
	require.True(full, "the window path must serve this page")
	require.Len(page, 7)
	for _, message := range page {
		assert.NotEqual(int64(90), message.ConversationID, "Cold-room match %d", message.ID)
	}
}

func TestSearchCommonWordPreservesPages(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	_, err := env.DB.ExecContext(t.Context(), `WITH RECURSIVE seq(x) AS (
		VALUES(1) UNION ALL SELECT x+1 FROM seq WHERE x < 3000
	) INSERT INTO messages (id, source_message_id, source_id, conversation_id, message_type, sent_at, subject, deleted_at, deleted_from_source_at)
	SELECT 100+x, 'window-' || x, 1, 1, 'email',
		datetime('2025-01-01', '+' || (x/5) || ' minutes'),
		CASE WHEN x > 1000 THEN 'visibility' ELSE '' END,
		CASE WHEN x > 1000 AND x % 10 BETWEEN 1 AND 4 THEN '2025-02-01' END,
		CASE WHEN x > 1000 AND x % 10 BETWEEN 5 AND 9 THEN '2025-02-01' END
	FROM seq`)
	require.NoError(t, err)
	env.EnableFTS()
	for _, scope := range []search.DeletionScope{"", search.DeletionScopeActive, search.DeletionScopeAny} {
		t.Run("visibility/"+string(scope), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			q := search.Parse("visibility")
			q.DeletionScope = scope
			conditions, args, ftsJoin := env.Engine.buildSearchQueryParts(env.Ctx, q)
			want, err := env.Engine.executeSearchQuery(env.Ctx, conditions, args, ftsJoin, 4000, 0)
			require.NoError(err)
			require.NotEmpty(want)
			var pages []MessageSummary
			for offset := 0; offset <= len(want)+7; offset += 7 {
				pages = append(pages, env.MustSearch(q, 7, offset)...)
			}
			assert.Equal(want, pages)
		})
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
