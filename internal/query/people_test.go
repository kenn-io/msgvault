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

// TestSearchPeopleAggregatesLinkedIdentitiesAsOneCanonicalRow pins the
// cluster-aware people search: a term matching only ONE member of a linked
// identity returns a single row keyed by the canonical (smallest member) ID
// with cluster-wide counts, files, date range, identifiers, and the shared
// best-name label — identical to the same identity's row in the unfiltered
// list. An entry naming several members counts once, and unlinked
// participants are unaffected.
func TestSearchPeopleAggregatesLinkedIdentitiesAsOneCanonicalRow(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("archive@example.com", "gmail")
	alicePrimary := b.AddParticipant("alice@example.com", "example.com", "Alice Example")
	aliceAlias := b.AddParticipant("alice@other.example", "other.example", "")
	bob := b.AddParticipant("bob@example.com", "example.com", "Bob Example")
	b.AddParticipantIdentifier(alicePrimary, "email", "alice@example.com", "alice@example.com", true)
	b.AddParticipantIdentifier(aliceAlias, "email", "alice@other.example", "alice@other.example", true)
	b.LinkCluster(alicePrimary, aliceAlias)

	start := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	primaryMail := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 401, Subject: "From primary", SentAt: start})
	b.AddFrom(primaryMail, alicePrimary, "Alice Example")
	aliasMail := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 402, Subject: "From alias", SentAt: start.Add(time.Hour)})
	b.AddFrom(aliasMail, aliceAlias, "")
	b.AddAttachmentWithMIME(901, aliasMail, 100, "alias.pdf", "application/pdf")
	// One entry addressed to BOTH members must count once for the cluster.
	bothMail := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 403, Subject: "To both", SentAt: start.Add(2 * time.Hour)})
	b.AddFrom(bothMail, bob, "Bob Example")
	b.AddTo(bothMail, alicePrimary, "Alice Example")
	b.AddTo(bothMail, aliceAlias, "")
	engine := b.BuildEngine()
	ctx := context.Background()

	searched, err := engine.SearchPeople(ctx, PersonSearchRequest{Query: "other.example", Page: PageSpec{Limit: 25}})
	requirements.NoError(err)
	requirements.Len(searched.Rows, 1, "a term matching one member must return the whole cluster once, never a split alias row")
	assertions.Equal(int64(1), searched.TotalCount)
	row := searched.Rows[0]
	assertions.Equal(alicePrimary, row.ID, "the row keys on the canonical (smallest member) participant ID")
	assertions.Equal("Alice Example", row.DisplayLabel, "the best name across the cluster labels the row")
	assertions.False(row.PartialLabel)
	assertions.Equal(int64(3), row.ActivityCount, "counts span every member; the entry naming both members counts once")
	assertions.Equal(int64(1), row.FileCount)
	assertions.Equal(start, row.FirstAt)
	assertions.Equal(start.Add(2*time.Hour), row.LastAt)
	assertions.Equal([]SourceCount{{SourceType: "gmail", Count: 3}}, row.SourceCounts)
	assertions.Equal([]PersonIdentifier{
		{Type: "email", Value: "alice@example.com", DisplayValue: "alice@example.com", IsPrimary: true, Provenance: "participant_identifiers", ParticipantID: alicePrimary},
		{Type: "email", Value: "alice@other.example", DisplayValue: "alice@other.example", IsPrimary: true, Provenance: "participant_identifiers", ParticipantID: aliceAlias},
	}, row.Identifiers, "identifiers span every cluster member, as the person detail does")

	unfiltered, err := engine.SearchPeople(ctx, PersonSearchRequest{Page: PageSpec{Limit: 25}})
	requirements.NoError(err)
	requirements.Len(unfiltered.Rows, 2, "the unfiltered list holds the cluster row and the unlinked participant")
	var unfilteredCluster *PersonSummary
	for i := range unfiltered.Rows {
		if unfiltered.Rows[i].ID == alicePrimary {
			unfilteredCluster = &unfiltered.Rows[i]
		}
	}
	requirements.NotNil(unfilteredCluster)
	assertions.Equal(*unfilteredCluster, row, "the searched row is identical to the unfiltered list's row for the same identity")

	bobSearch, err := engine.SearchPeople(ctx, PersonSearchRequest{Query: "bob", Page: PageSpec{Limit: 25}})
	requirements.NoError(err)
	requirements.Len(bobSearch.Rows, 1)
	assertions.Equal(bob, bobSearch.Rows[0].ID, "unlinked participants search exactly as before")
	assertions.Equal("Bob Example", bobSearch.Rows[0].DisplayLabel)
	assertions.Equal(int64(1), bobSearch.Rows[0].ActivityCount)
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

// TestSearchDomainsCountsLinkedAliasesAsOnePerson pins the cluster-aware
// domain person_count: two linked addresses on the same domain are ONE
// identity, so the domain counts them once — matching the cluster-aware
// People and Relationships views — while unlinked participants still count
// individually and per-entry metrics stay per-entry.
func TestSearchDomainsCountsLinkedAliasesAsOnePerson(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("archive@example.com", "gmail")
	alice := b.AddParticipant("alice@example.com", "example.com", "Alice Example")
	aliceAlias := b.AddParticipant("alice2@example.com", "example.com", "")
	bob := b.AddParticipant("bob@example.com", "example.com", "Bob Example")
	b.LinkCluster(alice, aliceAlias)

	when := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	toAlice := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 601, Subject: "To alice", SentAt: when})
	b.AddTo(toAlice, alice, "Alice Example")
	toAlias := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 602, Subject: "To alias", SentAt: when.Add(time.Hour)})
	b.AddTo(toAlias, aliceAlias, "")
	toBob := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 603, Subject: "To bob", SentAt: when.Add(2 * time.Hour)})
	b.AddTo(toBob, bob, "Bob Example")

	result, err := b.BuildEngine().SearchDomains(context.Background(), DomainSearchRequest{Page: PageSpec{Limit: 25}})
	requirements.NoError(err)
	requirements.Len(result.Rows, 1)
	assertions.Equal("example.com", result.Rows[0].Domain)
	assertions.Equal(int64(2), result.Rows[0].PersonCount, "linked same-domain aliases are one identity, not two")
	assertions.Equal(int64(3), result.Rows[0].ActivityCount, "activity stays per-entry")
}

// TestSearchDomainsCountsClusterWithCanonicalOnAnotherDomain guards the
// canonicalization order: a cluster whose canonical (smallest-ID) member is
// on ANOTHER domain still counts on this domain, because membership is the
// participant's own address — canonical resolution must never drop it.
func TestSearchDomainsCountsClusterWithCanonicalOnAnotherDomain(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("archive@example.com", "gmail")
	// Created first, so the work address is the smallest ID — the canonical.
	aliceWork := b.AddParticipant("alice@work.example", "work.example", "Alice (Work)")
	alice := b.AddParticipant("alice@example.com", "example.com", "Alice Example")
	b.LinkCluster(aliceWork, alice)

	when := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	toAlice := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 701, Subject: "To personal", SentAt: when})
	b.AddTo(toAlice, alice, "Alice Example")
	toWork := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 702, Subject: "To work", SentAt: when.Add(time.Hour)})
	b.AddTo(toWork, aliceWork, "Alice (Work)")

	result, err := b.BuildEngine().SearchDomains(context.Background(), DomainSearchRequest{
		Sort: SortSpec{Field: "display_label", Direction: "asc"}, Page: PageSpec{Limit: 25},
	})
	requirements.NoError(err)
	requirements.Len(result.Rows, 2)
	assertions.Equal("example.com", result.Rows[0].Domain)
	assertions.Equal(int64(1), result.Rows[0].PersonCount, "the identity counts here through its address on this domain")
	assertions.Equal("work.example", result.Rows[1].Domain)
	assertions.Equal(int64(1), result.Rows[1].PersonCount)
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

// TestGetPersonClusterLabelPrefersNamedMember pins the shared cluster label
// policy on the person header: with cluster member IDs supplied, the display
// label is the best non-empty display_name across ALL members (smallest
// participant ID wins ties deterministically) and PartialLabel turns false;
// with no named member the requested row's identifier fallback is unchanged;
// an unlinked participant is unaffected.
func TestGetPersonClusterLabelPrefersNamedMember(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")

	primary := b.AddParticipant("old@example.com", "example.com", "")
	alias := b.AddParticipant("new@example.com", "example.com", "Real Name")
	namelessA := b.AddParticipant("nameless-a@example.com", "example.com", "")
	namelessB := b.AddParticipant("nameless-b@example.com", "example.com", "")
	namedFirst := b.AddParticipant("named-first@example.com", "example.com", "First Name")
	namedSecond := b.AddParticipant("named-second@example.com", "example.com", "Second Name")

	sentAt := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	for _, senderID := range []int64{primary, namelessA, namedSecond} {
		message := b.AddMessage(MessageOpt{SourceID: source, Subject: "Activity", SentAt: sentAt})
		b.AddFrom(message, senderID, "")
	}
	engine := b.BuildEngine()
	ctx := context.Background()

	clustered, err := engine.GetPerson(ctx, primary, Context{}, []int64{primary, alias})
	requirements.NoError(err)
	requirements.NotNil(clustered)
	assertions.Equal("Real Name", clustered.DisplayLabel,
		"an unnamed member linked to a named alias must show the alias's name")
	assertions.False(clustered.PartialLabel, "a real cluster name is not a partial label")

	solo, err := engine.GetPerson(ctx, primary, Context{}, nil)
	requirements.NoError(err)
	requirements.NotNil(solo)
	assertions.Equal("old@example.com", solo.DisplayLabel, "an unlinked participant keeps its own fallback label")
	assertions.True(solo.PartialLabel)

	nameless, err := engine.GetPerson(ctx, namelessA, Context{}, []int64{namelessA, namelessB})
	requirements.NoError(err)
	requirements.NotNil(nameless)
	assertions.Equal("nameless-a@example.com", nameless.DisplayLabel,
		"with no named member, the requested row's identifier fallback is unchanged")
	assertions.True(nameless.PartialLabel)

	bothNamed, err := engine.GetPerson(ctx, namedSecond, Context{}, []int64{namedFirst, namedSecond})
	requirements.NoError(err)
	requirements.NotNil(bothNamed)
	assertions.Equal("First Name", bothNamed.DisplayLabel,
		"with several named members, the smallest participant ID's name wins deterministically")
	assertions.False(bothNamed.PartialLabel)
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

	people, err := engine.GetPersonSummary(context.Background(), person, explore, nil)
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

// TestGetPersonSummaryWithClusterMemberIDsCountsAliasOnlyActivity pins the
// cluster-aware contextual summary: when the analytical predicate matches
// activity owned ONLY by a linked alias, the summary for the canonical ID
// must still report that activity — with the cluster best-name label and
// cluster-wide identifiers — instead of returning no rows (a false 404).
// Without cluster member IDs the summary stays scoped to the requested
// participant alone, matching pre-cluster-aware behavior.
func TestGetPersonSummaryWithClusterMemberIDsCountsAliasOnlyActivity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	mailSource := b.AddSourceWithType("archive-a@example.com", "gmail")
	chatSource := b.AddSourceWithType("archive-b@example.com", "whatsapp")
	alicePrimary := b.AddParticipant("alice@example.com", "example.com", "Alice Example")
	aliceAlias := b.AddParticipant("alice@other.example", "other.example", "")
	b.AddParticipantIdentifier(alicePrimary, "email", "alice@example.com", "alice@example.com", true)
	b.AddParticipantIdentifier(aliceAlias, "email", "alice@other.example", "alice@other.example", true)

	start := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	primaryChat := b.AddMessage(MessageOpt{SourceID: chatSource, ConversationID: 501, SentAt: start, MessageType: "whatsapp", ConversationType: "direct_chat"})
	b.AddFrom(primaryChat, alicePrimary, "Alice Example")
	b.AddConversationParticipant(501, alicePrimary)
	// The predicate (mail source only) matches activity owned ONLY by the
	// linked alias, in a different source than the primary's chat message.
	aliasMail := b.AddMessage(MessageOpt{SourceID: mailSource, ConversationID: 502, Subject: "From alias", SentAt: start.Add(time.Hour)})
	b.AddFrom(aliasMail, aliceAlias, "")
	b.AddAttachmentWithMIME(902, aliasMail, 100, "alias.pdf", "application/pdf")
	engine := b.BuildEngine()
	ctx := context.Background()
	explore := ExploreRequest{Context: Context{SourceIDs: []int64{mailSource}}}

	clustered, err := engine.GetPersonSummary(ctx, alicePrimary, explore, []int64{alicePrimary, aliceAlias})
	requirements.NoError(err)
	requirements.Len(clustered.Rows, 1, "alias-only activity must keep the canonical identity present in the context")
	row := clustered.Rows[0]
	assertions.Equal(alicePrimary, row.ID)
	assertions.Equal("Alice Example", row.DisplayLabel, "the best name across the cluster labels the summary")
	assertions.False(row.PartialLabel)
	assertions.Equal(int64(1), row.ActivityCount, "the alias's in-context entry counts toward the canonical identity")
	assertions.Equal(int64(1), row.FileCount)
	assertions.Equal(start.Add(time.Hour), row.FirstAt)
	assertions.Equal(start.Add(time.Hour), row.LastAt)
	assertions.Equal([]SourceCount{{SourceType: "gmail", Count: 1}}, row.SourceCounts)
	assertions.Equal([]PersonIdentifier{
		{Type: "email", Value: "alice@example.com", DisplayValue: "alice@example.com", IsPrimary: true, Provenance: "participant_identifiers", ParticipantID: alicePrimary},
		{Type: "email", Value: "alice@other.example", DisplayValue: "alice@other.example", IsPrimary: true, Provenance: "participant_identifiers", ParticipantID: aliceAlias},
	}, row.Identifiers, "identifiers span every cluster member, as the person detail does")

	solo, err := engine.GetPersonSummary(ctx, alicePrimary, explore, nil)
	requirements.NoError(err)
	assertions.Empty(solo.Rows, "without cluster member IDs the summary stays scoped to the requested participant")
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
