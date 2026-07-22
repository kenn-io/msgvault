package query

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchPeopleKeepsSameNamePeopleSeparateAndUnifiesExplicitIdentifiers(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	emailSource := b.AddSourceWithType("archive-a@example.com", "gmail")
	chatSource := b.AddSourceWithType("archive-b@example.com", "apple_messages")
	first := b.AddParticipant("shared-one@example.com", "example.com", "Shared Name")
	second := b.AddParticipant("shared-two@example.com", "example.com", "Shared Name")
	b.AddParticipantIdentifier(first, "email", "shared-one@example.com", "shared-one@example.com", true)
	b.AddParticipantIdentifier(first, "phone", "+15550100001", "+1 555 010 0001", false)
	b.AddParticipantIdentifier(second, "email", "shared-two@example.com", "shared-two@example.com", true)

	oldest := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	firstMail := b.AddMessage(MessageOpt{SourceID: emailSource, ConversationID: 101, Subject: "First person mail", SentAt: oldest})
	b.AddFrom(firstMail, first, "Shared Name")
	b.AddAttachmentWithMIME(501, firstMail, 100, "first.pdf", "application/pdf")
	secondMail := b.AddMessage(MessageOpt{SourceID: emailSource, ConversationID: 102, Subject: "Second person mail", SentAt: oldest.Add(time.Hour)})
	b.AddFrom(secondMail, second, "Shared Name")
	chat := b.AddMessage(MessageOpt{SourceID: chatSource, ConversationID: 103, Subject: "", SentAt: oldest.Add(2 * time.Hour), MessageType: "imessage", ConversationType: "direct_chat"})
	b.AddFrom(chat, first, "Shared Name")
	b.AddConversationParticipant(103, first)

	result, err := b.BuildEngine().SearchPeople(context.Background(), PersonSearchRequest{
		Query: "Shared Name", Sort: SortSpec{Field: "display_label", Direction: "asc"},
		Page: PageSpec{Limit: 25},
	})
	requirements.NoError(err)
	requirements.Len(result.Rows, 2)
	assertions.Equal(int64(2), result.TotalCount)
	assertions.Equal([]int64{first, second}, []int64{result.Rows[0].ID, result.Rows[1].ID})
	assertions.Equal(int64(2), result.Rows[0].ActivityCount)
	assertions.Equal(int64(1), result.Rows[0].FileCount)
	assertions.Equal([]SourceCount{{SourceType: "apple_messages", Count: 1}, {SourceType: "gmail", Count: 1}}, result.Rows[0].SourceCounts)
	assertions.Equal(oldest, result.Rows[0].FirstAt)
	assertions.Equal(oldest.Add(2*time.Hour), result.Rows[0].LastAt)
	assertions.Equal([]PersonIdentifier{
		{Type: "email", Value: "shared-one@example.com", DisplayValue: "shared-one@example.com", IsPrimary: true, Provenance: "participant_identifiers", ParticipantID: first},
		{Type: "phone", Value: "+15550100001", DisplayValue: "+1 555 010 0001", Provenance: "participant_identifiers", ParticipantID: first},
	}, result.Rows[0].Identifiers)
	assertions.NotEqual(result.Rows[0].ID, result.Rows[1].ID, "display-name equality must never merge people")
	assertions.NotEmpty(result.CacheRevision)
}

func TestSearchPeopleShowsPartialIdentityLabelsHonestly(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("archive@example.com", "beeper")
	person := b.AddParticipant("", "", "   ")
	b.AddParticipantIdentifier(person, "matrix", "@user:example.invalid", "", true)
	message := b.AddMessage(MessageOpt{SourceID: source, Subject: "Unlabeled activity", SentAt: time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC), MessageType: "matrix"})
	b.AddFrom(message, person, "")

	result, err := b.BuildEngine().SearchPeople(context.Background(), PersonSearchRequest{Page: PageSpec{Limit: 25}})
	requirements.NoError(err)
	requirements.Len(result.Rows, 1)
	assertions.Equal("@user:example.invalid", result.Rows[0].DisplayLabel)
	assertions.True(result.Rows[0].PartialLabel)
	assertions.Equal("   ", result.Rows[0].DisplayName)
}

func TestSearchDomainsSpansSourcesAndModalitiesWithoutOrganizationClaims(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	emailSource := b.AddSourceWithType("archive-a@example.com", "gmail")
	chatSource := b.AddSourceWithType("archive-b@example.com", "whatsapp")
	first := b.AddParticipant("one@example.com", "example.com", "One")
	second := b.AddParticipant("two@example.com", "example.com", "Two")
	when := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	mail := b.AddMessage(MessageOpt{SourceID: emailSource, ConversationID: 201, Subject: "Mail", SentAt: when})
	b.AddFrom(mail, first, "One")
	chat := b.AddMessage(MessageOpt{SourceID: chatSource, ConversationID: 202, SentAt: when.Add(time.Hour), MessageType: "whatsapp", ConversationType: "group_chat"})
	b.AddFrom(chat, second, "Two")
	b.AddConversationParticipant(202, second)
	b.AddAttachmentWithMIME(601, chat, 200, "photo.jpg", "image/jpeg")

	result, err := b.BuildEngine().SearchDomains(context.Background(), DomainSearchRequest{
		Query: "EXAMPLE", Sort: SortSpec{Field: "activity_count", Direction: "desc"}, Page: PageSpec{Limit: 25},
	})
	requirements.NoError(err)
	requirements.Len(result.Rows, 1)
	assertions.Equal("example.com", result.Rows[0].Domain)
	assertions.Equal(int64(2), result.Rows[0].ActivityCount)
	assertions.Equal(int64(2), result.Rows[0].PersonCount)
	assertions.Equal(int64(1), result.Rows[0].FileCount)
	assertions.Equal([]SourceCount{{SourceType: "gmail", Count: 1}, {SourceType: "whatsapp", Count: 1}}, result.Rows[0].SourceCounts)
	assertions.Equal(when, result.Rows[0].FirstAt)
	assertions.Equal(when.Add(time.Hour), result.Rows[0].LastAt)
	assertions.NotEmpty(result.CacheRevision)
}

func TestGetPersonAndDomainAreExactAndBounded(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	person := b.AddParticipant("person@example.com", "example.com", "Person")
	b.AddParticipantIdentifier(person, "email", "person@example.com", "Person <person@example.com>", true)
	message := b.AddMessage(MessageOpt{SourceID: source, Subject: "One", SentAt: time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)})
	b.AddFrom(message, person, "Person")
	engine := b.BuildEngine()

	personResult, err := engine.GetPerson(context.Background(), person, Context{}, nil)
	requirements.NoError(err)
	requirements.NotNil(personResult)
	assertions.Equal(person, personResult.ID)
	assertions.Equal("email", personResult.Identifiers[0].Type)

	domainResult, err := engine.GetDomain(context.Background(), "EXAMPLE.COM", Context{})
	requirements.NoError(err)
	requirements.NotNil(domainResult)
	assertions.Equal("example.com", domainResult.Domain)

	missingPerson, err := engine.GetPerson(context.Background(), 9999, Context{}, nil)
	requirements.NoError(err)
	assertions.Nil(missingPerson)
	missingDomain, err := engine.GetDomain(context.Background(), "missing.example", Context{})
	requirements.NoError(err)
	assertions.Nil(missingDomain)
}

func TestGetPersonWithClusterMemberIDsSpansIdentifiersAndMetricsAcrossCluster(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	mailSource := b.AddSourceWithType("archive-a@example.com", "gmail")
	chatSource := b.AddSourceWithType("archive-b@example.com", "whatsapp")
	primary := b.AddParticipant("primary@example.com", "example.com", "Primary")
	secondary := b.AddParticipant("secondary@example.com", "example.com", "Secondary")
	b.AddParticipantIdentifier(primary, "email", "primary@example.com", "Primary <primary@example.com>", true)
	b.AddParticipantIdentifier(secondary, "phone", "+15550100002", "+1 555 010 0002", true)
	primaryAt := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	message := b.AddMessage(MessageOpt{SourceID: mailSource, ConversationID: 301, Subject: "Cluster", SentAt: primaryAt})
	b.AddFrom(message, primary, "Primary")
	// Activity and an attachment owned only by the linked alias, in a
	// different source, before and after the primary's single message.
	aliasEarlier := b.AddMessage(MessageOpt{SourceID: mailSource, ConversationID: 302, Subject: "Alias earlier", SentAt: primaryAt.Add(-time.Hour)})
	b.AddFrom(aliasEarlier, secondary, "Secondary")
	b.AddAttachmentWithMIME(801, aliasEarlier, 100, "alias.pdf", "application/pdf")
	aliasLater := b.AddMessage(MessageOpt{SourceID: chatSource, ConversationID: 303, SentAt: primaryAt.Add(time.Hour), MessageType: "whatsapp", ConversationType: "direct_chat"})
	b.AddFrom(aliasLater, secondary, "Secondary")
	b.AddConversationParticipant(303, secondary)
	engine := b.BuildEngine()

	solo, err := engine.GetPerson(context.Background(), primary, Context{}, nil)
	requirements.NoError(err)
	requirements.NotNil(solo)
	requirements.Len(solo.Identifiers, 1, "without cluster member IDs, identifiers stay scoped to the requested participant")
	assertions.Equal(int64(1), solo.ActivityCount, "without cluster member IDs, metrics stay scoped to the requested participant")
	assertions.Equal(int64(0), solo.FileCount)
	assertions.Equal(primaryAt, solo.FirstAt)
	assertions.Equal(primaryAt, solo.LastAt)

	clustered, err := engine.GetPerson(context.Background(), primary, Context{}, []int64{primary, secondary})
	requirements.NoError(err)
	requirements.NotNil(clustered)
	requirements.Len(clustered.Identifiers, 2, "with cluster member IDs, identifiers span every member")
	byParticipant := map[int64]PersonIdentifier{}
	for _, identifier := range clustered.Identifiers {
		byParticipant[identifier.ParticipantID] = identifier
	}
	requirements.Contains(byParticipant, primary)
	requirements.Contains(byParticipant, secondary)
	assertions.Equal("email", byParticipant[primary].Type)
	assertions.Equal("phone", byParticipant[secondary].Type)
	// Metrics aggregate every member: counts, files, date range, and source
	// coverage all include activity owned only by the linked alias, matching
	// what the cluster-aware relationship timeline shows.
	assertions.Equal(primary, clustered.ID)
	assertions.Equal(int64(3), clustered.ActivityCount)
	assertions.Equal(int64(1), clustered.FileCount)
	assertions.Equal(primaryAt.Add(-time.Hour), clustered.FirstAt)
	assertions.Equal(primaryAt.Add(time.Hour), clustered.LastAt)
	assertions.Equal([]SourceCount{{SourceType: "gmail", Count: 2}, {SourceType: "whatsapp", Count: 1}}, clustered.SourceCounts)

	// The filtered shape (an analytical context present) must widen the same
	// way: scoped to the mail source, the alias's mail entry still counts.
	filtered, err := engine.GetPerson(context.Background(), primary, Context{SourceIDs: []int64{mailSource}}, []int64{primary, secondary})
	requirements.NoError(err)
	requirements.NotNil(filtered)
	assertions.Equal(int64(2), filtered.ActivityCount)
	assertions.Equal(int64(1), filtered.FileCount)
	assertions.Equal(primaryAt.Add(-time.Hour), filtered.FirstAt)
	assertions.Equal(primaryAt, filtered.LastAt)
}

func TestContextualPersonAndDomainSummariesUseTheExactCanonicalPopulation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	mail := b.AddSourceWithType("mail@example.com", "gmail")
	chat := b.AddSourceWithType("chat@example.com", "matrix")
	person := b.AddParticipant("person@example.com", "example.com", "Person")
	firstAt := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	mailMessage := b.AddMessage(MessageOpt{SourceID: mail, Subject: "Included", SentAt: firstAt})
	chatMessage := b.AddMessage(MessageOpt{SourceID: chat, Subject: "Excluded", SentAt: firstAt.Add(time.Hour)})
	b.AddFrom(mailMessage, person, "Person")
	b.AddFrom(chatMessage, person, "Person")
	b.AddAttachmentWithMIME(701, mailMessage, 100, "included.pdf", "application/pdf")
	b.AddAttachmentWithMIME(702, chatMessage, 100, "excluded.pdf", "application/pdf")
	engine := b.BuildEngine()
	explore := ExploreRequest{Search: SearchSpec{
		Mode: SearchFullText, Query: "Included", CandidateMessageIDs: []int64{mailMessage}, LexicalIndexRevision: "fts5:context",
	}}

	people, err := engine.GetPersonSummary(context.Background(), person, explore)
	requirements.NoError(err)
	requirements.Len(people.Rows, 1)
	assertions.Equal(int64(1), people.Rows[0].ActivityCount)
	assertions.Equal(int64(1), people.Rows[0].FileCount)
	assertions.Equal(firstAt, people.Rows[0].FirstAt)
	assertions.Equal([]SourceCount{{SourceType: "gmail", Count: 1}}, people.Rows[0].SourceCounts)
	assertions.Equal(SearchProvenance{LexicalIndexRevision: "fts5:context"}, people.SearchProvenance)

	domains, err := engine.GetDomainSummary(context.Background(), "EXAMPLE.COM", explore)
	requirements.NoError(err)
	requirements.Len(domains.Rows, 1)
	assertions.Equal(int64(1), domains.Rows[0].ActivityCount)
	assertions.Equal(int64(1), domains.Rows[0].FileCount)
	assertions.Equal(firstAt, domains.Rows[0].FirstAt)
	assertions.Equal([]SourceCount{{SourceType: "gmail", Count: 1}}, domains.Rows[0].SourceCounts)
	assertions.Equal(SearchProvenance{LexicalIndexRevision: "fts5:context"}, domains.SearchProvenance)
}

func TestPeopleAndDomainSearchRejectUnboundedOrUnknownSorts(t *testing.T) {
	requirements := require.New(t)
	engine := NewTestDataBuilder(t).BuildEngine()
	_, err := engine.SearchPeople(context.Background(), PersonSearchRequest{Page: PageSpec{Limit: 501}})
	requirements.ErrorIs(err, ErrInvalidExploreRequest)
	_, err = engine.SearchDomains(context.Background(), DomainSearchRequest{Sort: SortSpec{Field: "sql", Direction: "asc"}})
	requirements.ErrorIs(err, ErrInvalidExploreRequest)
}

func TestSearchPeopleProjectsResolvedCanonicalSearchCandidates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	first := b.AddParticipant("first@example.com", "example.com", "Shared Name")
	second := b.AddParticipant("second@example.com", "example.com", "Shared Name")
	firstMessage := b.AddMessage(MessageOpt{SourceID: source, Subject: "First", SentAt: time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)})
	secondMessage := b.AddMessage(MessageOpt{SourceID: source, Subject: "Second", SentAt: time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)})
	b.AddFrom(firstMessage, first, "Shared Name")
	b.AddFrom(secondMessage, second, "Shared Name")
	engine := b.BuildEngine()
	generation := int64(17)

	for _, test := range []struct {
		name       string
		search     SearchSpec
		provenance SearchProvenance
	}{
		{name: "full text", search: SearchSpec{Mode: SearchFullText, Query: "Second", CandidateMessageIDs: []int64{secondMessage}, LexicalIndexRevision: "fts5:test"}, provenance: SearchProvenance{LexicalIndexRevision: "fts5:test"}},
		{name: "semantic", search: SearchSpec{Mode: SearchSemantic, Query: "Second", CandidateMessageIDs: []int64{secondMessage}, VectorGeneration: &generation}, provenance: SearchProvenance{VectorGeneration: &generation}},
		{name: "hybrid", search: SearchSpec{Mode: SearchHybrid, Query: "Second", CandidateMessageIDs: []int64{secondMessage}, LexicalCandidateMessageIDs: []int64{firstMessage, secondMessage}, LexicalIndexRevision: "fts5:test", VectorGeneration: &generation}, provenance: SearchProvenance{LexicalIndexRevision: "fts5:test", VectorGeneration: &generation}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := engine.SearchPeople(context.Background(), PersonSearchRequest{
				Explore: ExploreRequest{Search: test.search}, Query: "Shared Name", Page: PageSpec{Limit: 25},
			})
			requirements.NoError(err)
			requirements.Len(result.Rows, 1)
			assertions.Equal(second, result.Rows[0].ID, "same-name participant outside candidates must remain excluded")
			assertions.Equal(test.provenance, result.SearchProvenance)
		})
	}

	empty, err := engine.SearchPeople(context.Background(), PersonSearchRequest{
		Explore: ExploreRequest{Search: SearchSpec{Mode: SearchFullText, Query: "none", CandidateMessageIDs: []int64{}, LexicalIndexRevision: "fts5:empty"}},
		Page:    PageSpec{Limit: 25},
	})
	requirements.NoError(err)
	assertions.Empty(empty.Rows)
	assertions.Equal(int64(0), empty.TotalCount)
}

func TestSearchDomainsProjectsResolvedCanonicalSearchCandidates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	first := b.AddParticipant("first@example.com", "example.com", "First")
	second := b.AddParticipant("second@other.example", "other.example", "Second")
	firstMessage := b.AddMessage(MessageOpt{SourceID: source, Subject: "First", SentAt: time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)})
	secondMessage := b.AddMessage(MessageOpt{SourceID: source, Subject: "Second", SentAt: time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)})
	b.AddFrom(firstMessage, first, "First")
	b.AddFrom(secondMessage, second, "Second")

	result, err := b.BuildEngine().SearchDomains(context.Background(), DomainSearchRequest{
		Explore: ExploreRequest{Search: SearchSpec{Mode: SearchFullText, Query: "Second", CandidateMessageIDs: []int64{secondMessage}, LexicalIndexRevision: "fts5:domain"}},
		Page:    PageSpec{Limit: 25},
	})
	requirements.NoError(err)
	requirements.Len(result.Rows, 1)
	assertions.Equal("other.example", result.Rows[0].Domain)
	assertions.Equal(SearchProvenance{LexicalIndexRevision: "fts5:domain"}, result.SearchProvenance)
}
