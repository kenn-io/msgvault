package query

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentityActivityPreservesNonChatConversationEdgeSemantics(t *testing.T) {
	requirementsForTest := require.New(t)
	builder := NewTestDataBuilder(t)
	sourceID := builder.AddSourceWithType("archive@example.com", "gmail")
	direct := builder.AddParticipant("direct@example.com", "direct.example", "Direct")
	conversationOnly := builder.AddParticipant(
		"member@community.example",
		"community.example",
		"Conversation Member",
	)
	messageID := builder.AddMessage(MessageOpt{
		SourceID:       sourceID,
		ConversationID: 901,
		Subject:        "Non-chat membership",
		SentAt:         time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
		MessageType:    "email",
	})
	builder.AddFrom(messageID, direct, "Direct")
	builder.AddConversationParticipant(901, conversationOnly)
	engine := builder.BuildEngine()

	request := PersonSearchRequest{
		Explore: ExploreRequest{Context: Context{
			ParticipantIDs: []int64{conversationOnly},
		}},
		Page: PageSpec{Limit: 25},
	}
	people, err := engine.SearchPeople(context.Background(), request)
	requirementsForTest.NoError(err)
	requirementsForTest.Len(people.Rows, 1)
	assert.Equal(t, direct, people.Rows[0].ID,
		"conversation membership qualifies the fact filter but not non-chat people fan-out")
	engine.identityCandidateFastPathDisabled = true
	logicalPeople, err := engine.SearchPeople(context.Background(), request)
	requirementsForTest.NoError(err)
	assert.Equal(t, logicalPeople, people)
	engine.identityCandidateFastPathDisabled = false

	domains, err := engine.SearchDomains(context.Background(), DomainSearchRequest{
		Explore: ExploreRequest{Context: Context{
			ParticipantIDs: []int64{conversationOnly},
		}},
		Sort: SortSpec{Field: "display_label", Direction: "asc"},
		Page: PageSpec{Limit: 25},
	})
	requirementsForTest.NoError(err)
	requirementsForTest.Len(domains.Rows, 2)
	assert.Equal(t,
		[]string{"community.example", "direct.example"},
		[]string{domains.Rows[0].Domain, domains.Rows[1].Domain},
		"non-chat domain fan-out includes direct and conversation edges")
}

func TestUnfilteredIdentityIndexMatchesLegacyPeopleAndDomains(t *testing.T) {
	requirementsForTest := require.New(t)
	builder := NewTestDataBuilder(t)
	mailSource := builder.AddSourceWithType("archive-a@example.com", "gmail")
	chatSource := builder.AddSourceWithType("archive-b@example.com", "whatsapp")
	alice := builder.AddParticipant("alice@example.com", "example.com", "Alice")
	bob := builder.AddParticipant("bob@example.net", "example.net", "Bob")
	start := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	mail := builder.AddMessage(MessageOpt{
		SourceID: mailSource, ConversationID: 910, Subject: "Mail", SentAt: start,
	})
	builder.AddFrom(mail, alice, "Alice")
	builder.AddTo(mail, bob, "Bob")
	builder.AddAttachmentWithMIME(1, mail, 100, "mail.pdf", "application/pdf")
	chat := builder.AddMessage(MessageOpt{
		SourceID: chatSource, ConversationID: 911, SentAt: start.Add(time.Hour),
		MessageType: "whatsapp", ConversationType: "group_chat",
	})
	builder.AddFrom(chat, bob, "Bob")
	builder.AddConversationParticipant(911, alice)
	builder.AddConversationParticipant(911, bob)
	engine := builder.BuildEngine()

	peopleRequest := PersonSearchRequest{
		Sort: SortSpec{Field: "display_label", Direction: "asc"},
		Page: PageSpec{Limit: 25},
	}
	gotPeople, err := engine.SearchPeople(context.Background(), peopleRequest)
	requirementsForTest.NoError(err)
	wantPeople, err := engine.searchPeopleLegacy(
		context.Background(),
		peopleRequest,
		nil,
		nil,
	)
	requirementsForTest.NoError(err)
	assert.Equal(t, wantPeople, gotPeople)

	domainRequest := DomainSearchRequest{
		Sort: SortSpec{Field: "display_label", Direction: "asc"},
		Page: PageSpec{Limit: 25},
	}
	gotDomains, err := engine.SearchDomains(context.Background(), domainRequest)
	requirementsForTest.NoError(err)
	wantDomains, err := engine.searchDomainsLegacy(
		context.Background(),
		domainRequest,
		"",
	)
	requirementsForTest.NoError(err)
	assert.Equal(t, wantDomains, gotDomains)
}

func TestIdentityActivityMergesAuthorshipAcrossLinkedAliases(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	builder := NewTestDataBuilder(t)
	sourceID := builder.AddSourceWithType("archive@example.com", "gmail")
	author := builder.AddParticipant("author@example.com", "example.com", "Author")
	coRecipient := builder.AddParticipant(
		"author-alias@example.com",
		"example.com",
		"Author Alias",
	)
	builder.LinkCluster(author, coRecipient)
	messageID := builder.AddMessage(MessageOpt{
		SourceID: sourceID,
		Subject:  "One incoming entry",
		SentAt:   time.Date(2026, 7, 20, 11, 0, 0, 0, time.UTC),
	})
	builder.AddFrom(messageID, author, "Author")
	builder.AddTo(messageID, coRecipient, "Author Alias")
	engine := builder.BuildEngine()

	response, err := engine.SearchPeople(context.Background(), PersonSearchRequest{
		Explore: ExploreRequest{Context: Context{SourceIDs: []int64{sourceID}}},
		Page:    PageSpec{Limit: 25},
	})
	requirements.NoError(err)
	requirements.Len(response.Rows, 1)
	assertions.Equal(author, response.Rows[0].ID)
	assertions.Equal(int64(1), response.Rows[0].ActivityCount,
		"authored and co-recipient aliases merge into one canonical entry")
}

func TestSourceOnlyPeopleRollupFastPathMatchesLogicalReduction(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	builder := NewTestDataBuilder(t)
	selectedSource := builder.AddSourceWithType("selected@example.com", "gmail")
	otherSource := builder.AddSourceWithType("other@example.com", "gmail")
	selectedPerson := builder.AddParticipant(
		"selected-person@example.com",
		"example.com",
		"Selected Person",
	)
	otherPerson := builder.AddParticipant(
		"other-person@example.com",
		"example.com",
		"Other Person",
	)
	selectedMessage := builder.AddMessage(MessageOpt{
		SourceID: selectedSource,
		Subject:  "Selected",
		SentAt:   time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
	})
	builder.AddFrom(selectedMessage, selectedPerson, "Selected Person")
	builder.AddAttachmentWithMIME(
		1,
		selectedMessage,
		100,
		"selected.pdf",
		"application/pdf",
	)
	otherMessage := builder.AddMessage(MessageOpt{
		SourceID: otherSource,
		Subject:  "Other",
		SentAt:   time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC),
	})
	builder.AddFrom(otherMessage, otherPerson, "Other Person")
	engine := builder.BuildEngine()

	request := PersonSearchRequest{
		Explore: ExploreRequest{Context: Context{
			SourceIDs: []int64{selectedSource},
		}},
		Sort: SortSpec{Field: "display_label", Direction: "asc"},
		Page: PageSpec{Limit: 25},
	}
	fast, err := engine.SearchPeople(context.Background(), request)
	requirements.NoError(err)
	engine.sourceRollupFastPathDisabled = true
	logical, err := engine.SearchPeople(context.Background(), request)
	requirements.NoError(err)

	assertions.Equal(logical, fast)
	requirements.Len(fast.Rows, 1)
	assertions.Equal(selectedPerson, fast.Rows[0].ID)
	assertions.Equal(int64(1), fast.Rows[0].ActivityCount)
	assertions.Equal(int64(1), fast.Rows[0].FileCount)
	assertions.Equal([]SourceCount{{SourceType: "gmail", Count: 1}},
		fast.Rows[0].SourceCounts)
}

func TestIdentityActivityAppliesFactFilters(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	builder := NewTestDataBuilder(t)
	selectedSource := builder.AddSourceWithType("selected@example.com", "gmail")
	otherSource := builder.AddSourceWithType("other@example.com", "gmail")
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	deletedAt := start.Add(12 * time.Hour)

	add := func(
		email string,
		sourceID int64,
		occurredAt time.Time,
		messageType string,
		deleted bool,
	) int64 {
		personID := builder.AddParticipant(email, "example.com", email)
		opt := MessageOpt{
			SourceID:    sourceID,
			Subject:     email,
			SentAt:      occurredAt,
			MessageType: messageType,
		}
		if deleted {
			opt.DeletedFromSourceAt = &deletedAt
		}
		messageID := builder.AddMessage(opt)
		builder.AddFrom(messageID, personID, email)
		return messageID
	}

	target := add("target@example.com", selectedSource, start.Add(12*time.Hour), "email", true)
	add("wrong-source@example.com", otherSource, start.Add(12*time.Hour), "email", true)
	add("too-early@example.com", selectedSource, start.Add(-time.Hour), "email", true)
	add("too-late@example.com", selectedSource, end, "email", true)
	add("wrong-type@example.com", selectedSource, start.Add(12*time.Hour), "sms", true)
	add("active@example.com", selectedSource, start.Add(12*time.Hour), "email", false)
	add(
		"not-candidate@example.com",
		selectedSource,
		start.Add(12*time.Hour),
		"email",
		true,
	)
	engine := builder.BuildEngine()

	response, err := engine.SearchPeople(context.Background(), PersonSearchRequest{
		Explore: ExploreRequest{
			Context: Context{
				SourceIDs:    []int64{selectedSource},
				MessageTypes: []string{"email"},
				After:        &start,
				Before:       &end,
				Deletion:     DeletionDeleted,
			},
			Search: SearchSpec{
				Mode:                 SearchFullText,
				Query:                "target",
				CandidateMessageIDs:  []int64{target},
				LexicalIndexRevision: "fts5:identity-facts",
			},
		},
		Page: PageSpec{Limit: 25},
	})
	requirements.NoError(err)
	requirements.Len(response.Rows, 1)
	assertions.Equal("target@example.com", response.Rows[0].DisplayLabel)
	assertions.Equal(
		SearchProvenance{LexicalIndexRevision: "fts5:identity-facts"},
		response.SearchProvenance,
	)
}

func TestIdentityActivityDateFiltersBindUTCWallClock(t *testing.T) {
	assertionsForTest := assert.New(t)
	zone := time.FixedZone("fixture-offset", -4*60*60)
	after := time.Date(2026, 7, 20, 9, 30, 0, 0, zone)
	before := after.Add(2 * time.Hour)

	conditions, args := buildIdentityFactConditions(ExploreRequest{
		Context: Context{After: &after, Before: &before},
	})
	assertionsForTest.Contains(conditions, "f.occurred_at >= CAST(? AS TIMESTAMP)")
	assertionsForTest.Contains(conditions, "f.occurred_at < CAST(? AS TIMESTAMP)")
	require.Len(t, args, 2)
	assertionsForTest.Equal("2026-07-20 13:30:00", args[0])
	assertionsForTest.Equal("2026-07-20 15:30:00", args[1])
}

func TestIdentityCandidateNarrowingRetainsScalarFilters(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	builder := NewTestDataBuilder(t)
	selectedSource := builder.AddSourceWithType("selected@example.com", "gmail")
	otherSource := builder.AddSourceWithType("other@example.com", "gmail")
	personID := builder.AddParticipant("person@example.com", "example.com", "Person")
	selectedMessage := builder.AddMessage(MessageOpt{
		SourceID:    selectedSource,
		MessageType: "email",
		SentAt:      time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
	})
	builder.AddFrom(selectedMessage, personID, "Person")
	otherMessage := builder.AddMessage(MessageOpt{
		SourceID:    otherSource,
		MessageType: "email",
		SentAt:      time.Date(2026, 7, 20, 11, 0, 0, 0, time.UTC),
	})
	builder.AddFrom(otherMessage, personID, "Person")
	engine := builder.BuildEngine()

	request := ExploreRequest{Context: Context{
		SourceIDs:      []int64{selectedSource},
		ParticipantIDs: []int64{personID},
		MessageTypes:   []string{"email"},
		Deletion:       DeletionActive,
	}}
	narrowed, err := engine.narrowIdentityFactCandidates(
		context.Background(),
		request,
	)
	requirements.NoError(err)
	assertions.Equal([]int64{selectedMessage}, narrowed.Search.CandidateMessageIDs)
	assertions.Empty(narrowed.Context.ParticipantIDs)
	assertions.Equal(request.Context.SourceIDs, narrowed.Context.SourceIDs)
	assertions.Equal(request.Context.MessageTypes, narrowed.Context.MessageTypes)
	assertions.Equal(request.Context.Deletion, narrowed.Context.Deletion)
}

func TestIdentityCandidateNarrowingSaturationFallsBackExactly(t *testing.T) {
	builder := NewTestDataBuilder(t)
	sourceID := builder.AddSourceWithType("archive@example.com", "gmail")
	personID := builder.AddParticipant("person@example.com", "example.com", "Person")
	sentAt := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	for index := range MaxExploreCandidateMessageIDs + 1 {
		messageID := builder.AddMessage(MessageOpt{
			SourceID:    sourceID,
			MessageType: "email",
			SentAt:      sentAt.Add(time.Duration(index) * time.Second),
		})
		builder.AddFrom(messageID, personID, "Person")
	}
	engine := builder.BuildEngine()
	request := PersonSearchRequest{
		Explore: ExploreRequest{Context: Context{
			ParticipantIDs: []int64{personID},
		}},
		Sort: SortSpec{Field: "activity_count", Direction: "desc"},
		Page: PageSpec{Limit: 25},
	}

	narrowed, err := engine.narrowIdentityFactCandidates(
		context.Background(),
		request.Explore,
	)
	require.NoError(t, err)
	assert.Nil(t, narrowed.Search.CandidateMessageIDs)
	assert.Equal(t, request.Explore.Context.ParticipantIDs, narrowed.Context.ParticipantIDs)

	saturatedFallback, err := engine.SearchPeople(context.Background(), request)
	require.NoError(t, err)
	engine.identityCandidateFastPathDisabled = true
	generic, err := engine.SearchPeople(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, generic, saturatedFallback)
}

func TestIdentityEndpointsDoNotRequireLegacyAnalyticalViews(t *testing.T) {
	requirementsForTest := require.New(t)
	builder := NewTestDataBuilder(t)
	sourceID := builder.AddSourceWithType("archive@example.com", "gmail")
	personID := builder.AddParticipant("person@example.com", "example.com", "Indexed Person")
	messageID := builder.AddMessage(MessageOpt{
		SourceID:    sourceID,
		Subject:     "Indexed",
		SentAt:      time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC),
		MessageType: "email",
	})
	builder.AddFrom(messageID, personID, "Indexed Person")
	analyticsDir, cleanup := builder.Build()
	t.Cleanup(cleanup)

	engine, err := NewDuckDBEngine(
		analyticsDir,
		"",
		nil,
		DuckDBOptions{DisableLegacyAnalyticalViews: true},
	)
	requirementsForTest.NoError(err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })

	for _, view := range []string{"analytical_entries", "messages", "message_recipients"} {
		var count int64
		requirementsForTest.NoError(engine.db.QueryRow(`
			SELECT count(*)
			FROM duckdb_views()
			WHERE view_name = ?
		`, view).Scan(&count))
		assert.Zero(t, count, "%s must not be registered", view)
	}

	people, err := engine.SearchPeople(context.Background(), PersonSearchRequest{
		Query: "indexed",
		Page:  PageSpec{Limit: 25},
	})
	requirementsForTest.NoError(err)
	requirementsForTest.Len(people.Rows, 1)

	person, err := engine.GetPerson(context.Background(), personID, Context{}, nil)
	requirementsForTest.NoError(err)
	assert.Equal(t, personID, person.ID)

	personSummary, err := engine.GetPersonSummary(
		context.Background(),
		personID,
		ExploreRequest{Context: Context{SourceIDs: []int64{sourceID}}},
		nil,
	)
	requirementsForTest.NoError(err)
	requirementsForTest.Len(personSummary.Rows, 1)

	domains, err := engine.SearchDomains(context.Background(), DomainSearchRequest{
		Query: "example",
		Page:  PageSpec{Limit: 25},
	})
	requirementsForTest.NoError(err)
	requirementsForTest.Len(domains.Rows, 1)

	domain, err := engine.GetDomain(context.Background(), "example.com", Context{})
	requirementsForTest.NoError(err)
	assert.Equal(t, "example.com", domain.Domain)

	domainSummary, err := engine.GetDomainSummary(
		context.Background(),
		"example.com",
		ExploreRequest{Context: Context{SourceIDs: []int64{sourceID}}},
	)
	requirementsForTest.NoError(err)
	requirementsForTest.Len(domainSummary.Rows, 1)
}
