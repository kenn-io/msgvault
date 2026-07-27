package query

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentityActivityPreservesNonChatConversationEdgeSemantics(t *testing.T) {
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

	people, err := engine.SearchPeople(context.Background(), PersonSearchRequest{
		Explore: ExploreRequest{Context: Context{
			ParticipantIDs: []int64{conversationOnly},
		}},
		Page: PageSpec{Limit: 25},
	})
	require.NoError(t, err)
	require.Len(t, people.Rows, 1)
	assert.Equal(t, direct, people.Rows[0].ID,
		"conversation membership qualifies the fact filter but not non-chat people fan-out")

	domains, err := engine.SearchDomains(context.Background(), DomainSearchRequest{
		Explore: ExploreRequest{Context: Context{
			ParticipantIDs: []int64{conversationOnly},
		}},
		Sort: SortSpec{Field: "display_label", Direction: "asc"},
		Page: PageSpec{Limit: 25},
	})
	require.NoError(t, err)
	require.Len(t, domains.Rows, 2)
	assert.Equal(t,
		[]string{"community.example", "direct.example"},
		[]string{domains.Rows[0].Domain, domains.Rows[1].Domain},
		"non-chat domain fan-out includes direct and conversation edges")
}

func TestUnfilteredIdentityIndexMatchesLegacyPeopleAndDomains(t *testing.T) {
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
	require.NoError(t, err)
	wantPeople, err := engine.searchPeopleLegacy(
		context.Background(),
		peopleRequest,
		nil,
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, wantPeople, gotPeople)

	domainRequest := DomainSearchRequest{
		Sort: SortSpec{Field: "display_label", Direction: "asc"},
		Page: PageSpec{Limit: 25},
	}
	gotDomains, err := engine.SearchDomains(context.Background(), domainRequest)
	require.NoError(t, err)
	wantDomains, err := engine.searchDomainsLegacy(
		context.Background(),
		domainRequest,
		"",
	)
	require.NoError(t, err)
	assert.Equal(t, wantDomains, gotDomains)
}

func TestIdentityActivityMergesAuthorshipAcrossLinkedAliases(t *testing.T) {
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
	require.NoError(t, err)
	require.Len(t, response.Rows, 1)
	assert.Equal(t, author, response.Rows[0].ID)
	assert.Equal(t, int64(1), response.Rows[0].ActivityCount,
		"authored and co-recipient aliases merge into one canonical entry")
}

func TestIdentityActivityAppliesFactFilters(t *testing.T) {
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
	require.NoError(t, err)
	require.Len(t, response.Rows, 1)
	assert.Equal(t, "target@example.com", response.Rows[0].DisplayLabel)
	assert.Equal(t,
		SearchProvenance{LexicalIndexRevision: "fts5:identity-facts"},
		response.SearchProvenance,
	)
}

func TestIdentityEndpointsDoNotRequireLegacyAnalyticalViews(t *testing.T) {
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
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })

	for _, view := range []string{"analytical_entries", "messages", "message_recipients"} {
		var count int64
		require.NoError(t, engine.db.QueryRow(`
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
	require.NoError(t, err)
	require.Len(t, people.Rows, 1)

	person, err := engine.GetPerson(context.Background(), personID, Context{}, nil)
	require.NoError(t, err)
	assert.Equal(t, personID, person.ID)

	personSummary, err := engine.GetPersonSummary(
		context.Background(),
		personID,
		ExploreRequest{Context: Context{SourceIDs: []int64{sourceID}}},
		nil,
	)
	require.NoError(t, err)
	require.Len(t, personSummary.Rows, 1)

	domains, err := engine.SearchDomains(context.Background(), DomainSearchRequest{
		Query: "example",
		Page:  PageSpec{Limit: 25},
	})
	require.NoError(t, err)
	require.Len(t, domains.Rows, 1)

	domain, err := engine.GetDomain(context.Background(), "example.com", Context{})
	require.NoError(t, err)
	assert.Equal(t, "example.com", domain.Domain)

	domainSummary, err := engine.GetDomainSummary(
		context.Background(),
		"example.com",
		ExploreRequest{Context: Context{SourceIDs: []int64{sourceID}}},
	)
	require.NoError(t, err)
	require.Len(t, domainSummary.Rows, 1)
}
