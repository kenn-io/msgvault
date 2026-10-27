package query

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type domainPopulationProfile struct {
	Type      string                    `json:"type"`
	ExtraInfo map[string]any            `json:"extra_info"`
	Children  []domainPopulationProfile `json:"children"`
}

// Profile the public entry point so the regression also exercises dispatch
// and parameter binding, rather than manually selecting a query-builder mode.
func profileDomainPopulation(t *testing.T, engine *DuckDBEngine, request ExploreRequest) (domainPopulationProfile, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "listing.json")
	_, err := engine.db.Exec("SET enable_profiling = 'json'")
	require.NoError(t, err)
	_, err = engine.db.Exec("SET profiling_output = '" + escapePath(path) + "'")
	require.NoError(t, err)
	_, exploreErr := engine.Explore(t.Context(), request)
	_, err = engine.db.Exec("PRAGMA disable_profiling")
	require.NoError(t, err)
	if exploreErr != nil {
		return domainPopulationProfile{}, exploreErr
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var profile struct {
		Operators []domainPopulationProfile `json:"operator"`
	}
	require.NoError(t, json.Unmarshal(data, &profile))
	require.NotEmpty(t, profile.Operators)
	return domainPopulationProfile{Children: profile.Operators}, nil
}

func (p domainPopulationProfile) domainEvaluations() int {
	count := 0
	if strings.TrimSpace(p.Type) == "FILTER" {
		expression, _ := p.ExtraInfo["Expression"].(string)
		if strings.Contains(expression, "sender_domain") {
			count++
		}
	}
	for _, child := range p.Children {
		count += child.domainEvaluations()
	}
	return count
}

// The domain predicate joins recipients and conversation rosters. Regardless
// of its spelling or which domain-group field carries it, those joins must
// not be rebuilt for page selection, membership and total counting.
func FuzzExploreDomainPopulationShared(f *testing.F) {
	f.Add("example.com", false)
	f.Add("EXAMPLE.COM", true)
	f.Add("corp.example", false)
	f.Add("missing.example", true)
	f.Add("quote'\x00.example", false)
	f.Fuzz(func(t *testing.T, domain string, additionalOnly bool) {
		engine, _ := buildFastPathEquivalenceEngine(t)
		filter := Context{Domains: []string{domain}}
		if additionalOnly {
			filter = Context{AdditionalDomainGroups: [][]string{{domain}}}
		}
		request := ExploreRequest{Context: filter, Page: PageSpec{Limit: 3}}
		profile, err := profileDomainPopulation(t, engine, request)
		if err != nil {
			// Native parameter binding can reject invalid UTF-8. Preserve the
			// legacy rejection rather than broadening the accepted domain here.
			engine.exploreFastPathDisabled = true
			_, legacyErr := engine.Explore(t.Context(), request)
			require.EqualError(t, legacyErr, err.Error())
			return
		}
		// A predicate that cannot match may disappear entirely through Parquet
		// statistics pruning. Any remaining domain predicate is evaluated once.
		assert.LessOrEqual(t, profile.domainEvaluations(), 1)
	})
}

func TestExploreDomainPopulationKeepsMembershipAndPages(t *testing.T) {
	b := NewTestDataBuilder(t)
	source := b.AddSource("owner@example.com")
	alpha := b.AddParticipant("sender@alpha.example", "alpha.example", "Alpha")
	beta := b.AddParticipant("recipient@beta.example", "beta.example", "Beta")
	other := b.AddParticipant("person@other.example", "other.example", "Other")
	rosterOnly := b.AddParticipant("member@roster.example", "roster.example", "Roster")
	excluded := b.AddParticipant("sender@excluded.example", "excluded.example", "Excluded")
	base := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	addEmail := func(sender int64, minute int) int64 {
		return b.AddMessage(MessageOpt{SourceID: source, SenderID: &sender, SentAt: base.Add(time.Duration(minute) * time.Minute)})
	}
	senderMatch := addEmail(alpha, 1)
	b.AddTo(senderMatch, beta, "Beta")
	recipientMatch := addEmail(other, 2)
	b.AddTo(recipientMatch, alpha, "Alpha")
	b.AddCc(recipientMatch, beta, "Beta")
	const rosterConversation = int64(900)
	rosterMatch := b.AddMessage(MessageOpt{
		SourceID: source, SenderID: &other, SentAt: base.Add(3 * time.Minute),
		MessageType: "imessage", ConversationType: "group_chat", ConversationID: rosterConversation,
	})
	b.AddConversationParticipant(rosterConversation, alpha)
	b.AddConversationParticipant(rosterConversation, beta)
	b.AddConversationParticipant(rosterConversation, rosterOnly)
	b.AddMessage(MessageOpt{
		SourceID: source, SenderID: &excluded, SentAt: base.Add(10 * time.Minute),
		MessageType: "imessage", ConversationType: "group_chat", ConversationID: rosterConversation,
	})
	before := base.Add(7 * time.Minute)
	addEmail(alpha, 4)            // Matches only the primary group.
	betaOnly := addEmail(beta, 5) // Matches only the additional group.
	addEmail(other, 6)
	engine := b.BuildEngine()

	messageKey := func(id int64) string { return fmt.Sprintf("source:%d:message:msg%d", source, id) }
	rosterKey := fmt.Sprintf("source:%d:conversation:%d", source, rosterConversation)
	intersection := Context{Before: &before, Domains: []string{"ALPHA.EXAMPLE", "absent.example"}, AdditionalDomainGroups: [][]string{{"beta.example"}}}
	for _, tc := range []struct {
		name    string
		context Context
		page    PageSpec
		keys    []string
		count   int64
	}{
		{"intersection", intersection, PageSpec{Limit: 50}, []string{rosterKey, messageKey(recipientMatch), messageKey(senderMatch)}, 3},
		{"middle page", intersection, PageSpec{Limit: 1, Offset: 1}, []string{messageKey(recipientMatch)}, 3},
		{"at end", intersection, PageSpec{Limit: 1, Offset: 3}, []string{}, 3},
		{"beyond end", intersection, PageSpec{Limit: 1, Offset: 30}, []string{}, 3},
		{"additional group alone", Context{Before: &before, AdditionalDomainGroups: [][]string{{"beta.example"}}}, PageSpec{Limit: 50}, []string{messageKey(betaOnly), rosterKey, messageKey(recipientMatch), messageKey(senderMatch)}, 4},
		{"no matches", Context{Domains: []string{"absent.example"}}, PageSpec{Limit: 50}, []string{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			result, err := engine.Explore(t.Context(), ExploreRequest{Context: tc.context, Page: tc.page})
			require.NoError(err)
			keys := make([]string, 0, len(result.Rows))
			for _, row := range result.Rows {
				keys = append(keys, row.Key)
			}
			assert.Equal(tc.keys, keys)
			assert.Equal(tc.count, result.TotalCount)
			if tc.name == "intersection" {
				require.Len(result.Rows, 3)
				require.NotNil(result.Rows[0].AnchorMessageID)
				assert.Equal(rosterMatch, *result.Rows[0].AnchorMessageID)
				assert.ElementsMatch([]int64{alpha, beta, other, rosterOnly}, result.Rows[0].ParticipantIDs)
				assert.Equal(int64(1), result.Rows[0].MessageCount)
				assert.ElementsMatch([]string{"Alpha", "Beta", "Other", "Roster"}, result.Rows[0].ParticipantLabels)
			}
		})
	}
}

func (p domainPopulationProfile) hasFilteredPopulation() bool {
	if p.ExtraInfo["CTE Name"] == "filtered" {
		return true
	}
	for _, child := range p.Children {
		if child.hasFilteredPopulation() {
			return true
		}
	}
	return false
}

func TestExploreEmptyDomainGroupsKeepScalarRescans(t *testing.T) {
	engine, _ := buildFastPathEquivalenceEngine(t)
	for _, tc := range []struct {
		name   string
		filter Context
		shared bool
	}{
		{"unfiltered", Context{}, false},
		{"empty groups", Context{AdditionalDomainGroups: [][]string{nil, {}}}, false},
		{"nonempty among empty groups", Context{AdditionalDomainGroups: [][]string{nil, {"example.com"}, {}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := profileDomainPopulation(t, engine, ExploreRequest{Context: tc.filter, Page: PageSpec{Limit: 3}})
			require.NoError(t, err)
			assert.Equal(t, tc.shared, profile.hasFilteredPopulation())
		})
	}
}

// Domain groups intersect per message before chat fragments are collapsed.
// Matching different groups in separate fragments must not qualify the chat.
func TestExploreDomainIntersectionIsPerMessage(t *testing.T) {
	b := NewTestDataBuilder(t)
	source := b.AddSource("owner@example.com")
	alpha := b.AddParticipant("person@alpha.example", "alpha.example", "Alpha")
	beta := b.AddParticipant("person@beta.example", "beta.example", "Beta")
	for _, sender := range []int64{alpha, beta} {
		b.AddMessage(MessageOpt{SourceID: source, SenderID: &sender,
			MessageType: "imessage", ConversationType: "group_chat", ConversationID: 900})
	}
	engine := b.BuildEngine()
	result, err := engine.Explore(t.Context(), ExploreRequest{Context: Context{
		Domains: []string{"alpha.example"}, AdditionalDomainGroups: [][]string{{"beta.example"}},
	}})
	require.NoError(t, err)
	assert.Empty(t, result.Rows)
	assert.Zero(t, result.TotalCount)
}

func FuzzExploreDomainGroupFormsAgree(f *testing.F) {
	f.Add("example.com", 1, 0)
	f.Add("EXAMPLE.COM", 2, 1)
	f.Add("corp.example", 0, 500)
	f.Add("missing.example", 1000, 0)
	f.Add("\xc5XAMPLE.COM", 1, 0)
	f.Fuzz(func(t *testing.T, domain string, limit, offset int) {
		if limit < 0 || limit > maxExploreLimit || offset < 0 {
			t.Skip("pagination is outside Explore's supported range")
		}

		engine, _ := buildFastPathEquivalenceEngine(t)
		page := PageSpec{Limit: limit, Offset: offset}
		primaryRequest := ExploreRequest{
			Context: Context{Domains: []string{domain}}, Page: page,
		}
		primary, primaryErr := engine.Explore(t.Context(), primaryRequest)
		additional, additionalErr := engine.Explore(t.Context(), ExploreRequest{
			Context: Context{AdditionalDomainGroups: [][]string{nil, {domain}, {}}}, Page: page,
		})
		if !utf8.ValidString(domain) {
			require.Error(t, primaryErr, "invalid UTF-8 domain must be rejected")
		}
		if primaryErr != nil {
			require.EqualError(t, additionalErr, primaryErr.Error())
			engine.exploreFastPathDisabled = true
			_, legacyErr := engine.Explore(t.Context(), primaryRequest)
			require.EqualError(t, legacyErr, primaryErr.Error())
			return
		}
		require.NoError(t, additionalErr)
		assert.Equal(t, primary, additional)
	})
}
