package query

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonFileMatchCTEsBoundsRoleAggregationToAttachmentMessages(t *testing.T) {
	requirements := require.New(t)
	db, err := sql.Open("duckdb", "")
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(db.Close()) })

	_, err = db.Exec(`
		CREATE TABLE messages AS
		SELECT i::BIGINT AS id, 2::BIGINT AS sender_id, 700::BIGINT AS conversation_id, false AS is_from_me
		FROM generate_series(1, 100000) rows(i);
		CREATE TABLE conversations(id BIGINT, conversation_type VARCHAR);
		INSERT INTO conversations VALUES (700, 'channel');
		CREATE TABLE conversation_participants(conversation_id BIGINT, participant_id BIGINT);
		INSERT INTO conversation_participants VALUES (700, 1);
		CREATE TABLE message_recipients(message_id BIGINT, participant_id BIGINT, recipient_type VARCHAR);
		CREATE TABLE attachments(attachment_id BIGINT, message_id BIGINT);
		INSERT INTO attachments VALUES (1, 100000);
	`)
	requirements.NoError(err)

	ctes, args := personFileMatchCTEs(PersonFileScope{
		ParticipantIDs: []int64{1},
		Directions:     []PersonFileDirection{PersonFileGroup},
	})
	var aggregatedMessages int64
	err = db.QueryRowContext(context.Background(),
		"WITH "+ctes+" SELECT COUNT(*) FROM person_matches_unfiltered", args...).Scan(&aggregatedMessages)
	requirements.NoError(err)
	requirements.Equal(int64(1), aggregatedMessages,
		"role aggregation must exclude rostered channel messages without attachments")
}

func TestSearchFilesAppliesCanonicalContextAndFileFilters(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	sourceA := b.AddSourceWithType("archive-a@example.com", "gmail")
	sourceB := b.AddSourceWithType("archive-b@example.com", "imap")
	person := b.AddParticipant("alice@example.com", "example.com", "Alice Example")
	other := b.AddParticipant("bob@example.net", "example.net", "Bob Example")
	inside := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	after := inside.Add(-time.Hour)
	before := inside.Add(time.Hour)
	first := b.AddMessage(MessageOpt{SourceID: sourceA, ConversationID: 71, Subject: "Containing item", SentAt: inside})
	b.AddFrom(first, person, "Alice")
	b.AddAttachmentWithMIME(41, first, 2048, "Quarterly Report.PDF", "application/pdf")
	second := b.AddMessage(MessageOpt{SourceID: sourceA, ConversationID: 72, Subject: "Wrong person", SentAt: inside})
	b.AddFrom(second, other, "Bob")
	b.AddAttachmentWithMIME(42, second, 1024, "quarterly-report.pdf", "application/pdf")
	third := b.AddMessage(MessageOpt{SourceID: sourceB, ConversationID: 73, Subject: "Wrong source", SentAt: inside})
	b.AddFrom(third, person, "Alice")
	b.AddAttachmentWithMIME(43, third, 512, "quarterly-report.pdf", "application/pdf")

	result, err := b.BuildEngine().SearchFiles(context.Background(), FileSearchRequest{
		Explore: ExploreRequest{Context: Context{
			SourceIDs: []int64{sourceA}, ParticipantIDs: []int64{person}, Domains: []string{"example.com"},
			After: &after, Before: &before,
		}},
		FilenameQuery: "report", MIMEFamilies: []FileMIMEFamily{FileMIMEPDF},
		Sort: SortSpec{Field: "filename", Direction: "asc"}, Page: PageSpec{Limit: 25},
	})
	requirements.NoError(err)
	requirements.Len(result.Files, 1)
	file := result.Files[0]
	assertions.Equal(int64(41), file.ID)
	assertions.Equal("source:1:message:msg1:file:41", file.Key)
	assertions.Equal("source:1:message:msg1", file.EntryKey)
	assertions.Equal(int64(71), file.ConversationID)
	assertions.Equal("Containing item", file.ContainingTitle)
	assertions.Equal("application/pdf", file.MimeType)
	assertions.Equal(FileMIMEPDF, file.MIMEFamily)
	assertions.Equal([]int64{person}, file.ParticipantIDs)
	assertions.Equal([]string{"Alice Example"}, file.ParticipantLabels)
	assertions.Equal(int64(1), result.TotalCount)
	assertions.NotEmpty(result.CacheRevision)
}

func TestSearchFilesRestrictsVisualCandidateAttachmentIDsAfterHardFilters(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	message := b.AddMessage(MessageOpt{SourceID: source, Subject: "Visual candidates"})
	b.AddAttachmentWithMIME(71, message, 10, "first.png", "image/png")
	b.AddAttachmentWithMIME(72, message, 20, "second.pdf", "application/pdf")
	b.AddAttachmentWithMIME(73, message, 30, "third.png", "image/png")

	result, err := b.BuildEngine().SearchFiles(context.Background(), FileSearchRequest{
		AttachmentIDs: []int64{72, 73}, MIMEFamilies: []FileMIMEFamily{FileMIMEImage},
		Sort: SortSpec{Field: "filename", Direction: "asc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(result.Files, 1)
	assertions.Equal(int64(73), result.Files[0].ID)
	assertions.Equal(int64(1), result.TotalCount)
}

func TestFilesIdentityPredicateSeparatesDirectionsAndSources(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	sourceA := b.AddSource("archive-a@example.com")
	sourceB := b.AddSource("archive-b@example.com")
	identityID := b.AddParticipant("identity@example.com", "example.com", "Identity")
	otherID := b.AddParticipant("other@example.net", "example.net", "Other")
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	sent := b.AddMessage(MessageOpt{SourceID: sourceA, Subject: "sent", SentAt: base})
	b.AddFrom(sent, identityID, "Identity")
	b.AddTo(sent, otherID, "Other")
	b.AddAttachmentWithMIME(801, sent, 10, "sent.pdf", "application/pdf")
	received := b.AddMessage(MessageOpt{SourceID: sourceA, Subject: "received", SentAt: base.Add(-time.Hour)})
	b.AddFrom(received, otherID, "Other")
	b.AddRecipient(received, identityID, "bcc", "Identity")
	b.AddAttachmentWithMIME(802, received, 20, "received.pdf", "application/pdf")
	otherSource := b.AddMessage(MessageOpt{SourceID: sourceB, Subject: "other source", SentAt: base.Add(time.Hour)})
	b.AddFrom(otherSource, identityID, "Identity")
	b.AddAttachmentWithMIME(803, otherSource, 30, "other-source.pdf", "application/pdf")

	engine := b.BuildEngine()
	search := func(direction IdentityDirection) *FileSearchResponse {
		result, err := engine.SearchFiles(context.Background(), FileSearchRequest{
			Explore: ExploreRequest{Context: Context{Identity: &IdentityPredicate{
				SourceID: sourceA, ParticipantIDs: []int64{identityID}, Direction: direction,
			}}},
			Sort: SortSpec{Field: sortFieldOccurredAt, Direction: "desc"}, Page: PageSpec{Limit: 10},
		})
		requirements.NoError(err)
		return result
	}

	sender := search(IdentityDirectionSender)
	requirements.Len(sender.Files, 1)
	assertions.Equal("sent.pdf", sender.Files[0].Filename)
	assertions.Equal(sourceA, sender.Files[0].SourceID)
	assertions.Equal(int64(1), sender.TotalCount)

	recipient := search(IdentityDirectionRecipient)
	requirements.Len(recipient.Files, 1)
	assertions.Equal("received.pdf", recipient.Files[0].Filename)
	assertions.Equal(sourceA, recipient.Files[0].SourceID)
	assertions.Equal(int64(1), recipient.TotalCount)

	anyDirection := search(IdentityDirectionAny)
	requirements.Len(anyDirection.Files, 2)
	assertions.Equal([]string{"sent.pdf", "received.pdf"}, []string{anyDirection.Files[0].Filename, anyDirection.Files[1].Filename})
	assertions.Equal(int64(2), anyDirection.TotalCount)

	grouped, err := engine.GroupFiles(context.Background(), FileGroupRequest{
		Explore: ExploreRequest{Context: Context{Identity: &IdentityPredicate{
			SourceID: sourceA, ParticipantIDs: []int64{identityID}, Direction: IdentityDirectionAny,
		}}},
		Dimension: "source", Sort: SortSpec{Field: "count", Direction: "desc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(grouped.Rows, 1)
	assertions.Equal("1", grouped.Rows[0].Key)
	assertions.Equal(int64(2), grouped.Rows[0].Count)
}

func TestSearchFilesPersonScopeSeparatesDirectionsAndPreservesProvenance(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	person := b.AddParticipant("person@example.com", "example.com", "Person")
	alias := b.AddParticipant("person.alias@example.net", "example.net", "Person Alias")
	other := b.AddParticipant("other@example.org", "example.org", "Other")
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	groupConversation := int64(701)
	incoming := b.AddMessage(MessageOpt{
		SourceID: source, ConversationID: groupConversation, ConversationType: "group_chat",
		Subject: "from person", SentAt: base, SenderID: &person,
	})
	b.AddFrom(incoming, person, "Person")
	b.AddConversationParticipant(groupConversation, person)
	b.AddConversationParticipant(groupConversation, other)
	b.AddAttachmentWithMIME(901, incoming, 10, "from-person.png", "image/png")

	outgoing := b.AddMessage(MessageOpt{
		SourceID: source, Subject: "to person", SentAt: base.Add(-time.Hour), SenderID: &other,
	})
	b.AddTo(outgoing, alias, "Person Alias")
	b.AddCc(outgoing, person, "Person")
	b.AddRecipient(outgoing, alias, "bcc", "Person Alias")
	b.AddAttachmentWithMIME(902, outgoing, 20, "to-person.pdf", "application/pdf")

	groupOnlyConversation := int64(702)
	groupOnly := b.AddMessage(MessageOpt{
		SourceID: source, ConversationID: groupOnlyConversation, ConversationType: "channel",
		Subject: "group exchange", SentAt: base.Add(-2 * time.Hour), SenderID: &other,
	})
	b.AddConversationParticipant(groupOnlyConversation, person)
	b.AddConversationParticipant(groupOnlyConversation, other)
	b.AddAttachmentWithMIME(903, groupOnly, 30, "group-only.mp4", "video/mp4")

	directConversation := int64(703)
	directRosterOnly := b.AddMessage(MessageOpt{
		SourceID: source, ConversationID: directConversation, ConversationType: "direct_chat",
		Subject: "direct roster", SentAt: base.Add(-3 * time.Hour), SenderID: &other,
	})
	b.AddConversationParticipant(directConversation, person)
	b.AddAttachmentWithMIME(904, directRosterOnly, 40, "direct-roster.txt", "text/plain")

	legacyConversation := int64(704)
	legacyRosterOnly := b.AddMessage(MessageOpt{
		SourceID: source, ConversationID: legacyConversation, ConversationType: "legacy_chat",
		Subject: "legacy roster", SentAt: base.Add(-4 * time.Hour), SenderID: &other,
	})
	b.AddConversationParticipant(legacyConversation, person)
	b.AddAttachmentWithMIME(907, legacyRosterOnly, 45, "legacy-roster.txt", "text/plain")

	deletedAt := base.Add(time.Hour)
	deletedFromSource := b.AddMessage(MessageOpt{
		SourceID: source, Subject: "deleted from source", SentAt: base.Add(time.Hour),
		SenderID: &person, DeletedFromSourceAt: &deletedAt,
	})
	b.AddAttachmentWithMIME(905, deletedFromSource, 50, "deleted-source.pdf", "application/pdf")
	internallyDeleted := b.AddMessage(MessageOpt{
		SourceID: source, Subject: "internally deleted", SentAt: base.Add(2 * time.Hour),
		SenderID: &person, InternalDeletedAt: &deletedAt,
	})
	b.AddAttachmentWithMIME(906, internallyDeleted, 60, "deleted-internal.pdf", "application/pdf")

	engine := b.BuildEngine()
	search := func(directions ...PersonFileDirection) *FileSearchResponse {
		result, err := engine.SearchFiles(context.Background(), FileSearchRequest{
			Explore: ExploreRequest{Context: Context{Deletion: DeletionActive}},
			Person:  &PersonFileScope{ParticipantIDs: []int64{person, alias}, Directions: directions},
			Sort:    SortSpec{Field: sortFieldOccurredAt, Direction: "desc"}, Page: PageSpec{Limit: 10},
		})
		requirements.NoError(err)
		return result
	}

	fromPerson := search(PersonFileFromPerson)
	requirements.Len(fromPerson.Files, 1)
	assertions.Equal("from-person.png", fromPerson.Files[0].Filename)
	requirements.NotNil(fromPerson.Files[0].PersonProvenance)
	assertions.Equal(&PersonFileProvenance{
		ParticipantIDs: []int64{person},
		Roles:          []PersonFileRole{PersonFileRoleFrom, PersonFileRoleConversationMember},
		Directions:     []PersonFileDirection{PersonFileFromPerson, PersonFileGroup},
	}, fromPerson.Files[0].PersonProvenance)

	toPerson := search(PersonFileToPerson)
	requirements.Len(toPerson.Files, 2)
	assertions.Equal([]string{"to-person.pdf", "direct-roster.txt"}, []string{
		toPerson.Files[0].Filename, toPerson.Files[1].Filename,
	})
	assertions.Equal(&PersonFileProvenance{
		ParticipantIDs: []int64{person, alias},
		Roles:          []PersonFileRole{PersonFileRoleTo, PersonFileRoleCC, PersonFileRoleBCC},
		Directions:     []PersonFileDirection{PersonFileToPerson},
	}, toPerson.Files[0].PersonProvenance)
	assertions.Equal(&PersonFileProvenance{
		ParticipantIDs: []int64{person}, Roles: []PersonFileRole{PersonFileRoleTo},
		Directions: []PersonFileDirection{PersonFileToPerson},
	}, toPerson.Files[1].PersonProvenance)

	group := search(PersonFileGroup)
	requirements.Len(group.Files, 2)
	assertions.Equal([]string{"from-person.png", "group-only.mp4"},
		[]string{group.Files[0].Filename, group.Files[1].Filename})
	assertions.Equal(&PersonFileProvenance{
		ParticipantIDs: []int64{person}, Roles: []PersonFileRole{PersonFileRoleConversationMember},
		Directions: []PersonFileDirection{PersonFileGroup},
	}, group.Files[1].PersonProvenance)

	allDirections := search(PersonFileFromPerson, PersonFileToPerson, PersonFileGroup)
	requirements.Len(allDirections.Files, 4)
	assertions.Equal([]int64{901, 902, 903, 904}, []int64{
		allDirections.Files[0].ID, allDirections.Files[1].ID,
		allDirections.Files[2].ID, allDirections.Files[3].ID,
	})
	assertions.Equal(int64(4), allDirections.TotalCount)

	defaultScope, err := engine.SearchFiles(context.Background(), FileSearchRequest{
		Explore: ExploreRequest{Context: Context{Deletion: DeletionActive}},
		Person: &PersonFileScope{
			ParticipantIDs: []int64{person, alias},
			Directions: []PersonFileDirection{
				PersonFileFromPerson, PersonFileToPerson, PersonFileGroup,
			},
			IncludeUnclassifiedRosterRows: true,
		},
		Sort: SortSpec{Field: sortFieldOccurredAt, Direction: "desc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(defaultScope.Files, 5)
	assertions.Equal([]int64{901, 902, 903, 904, 907}, []int64{
		defaultScope.Files[0].ID, defaultScope.Files[1].ID, defaultScope.Files[2].ID,
		defaultScope.Files[3].ID, defaultScope.Files[4].ID,
	})
	assertions.Equal(&PersonFileProvenance{
		ParticipantIDs: []int64{person}, Roles: []PersonFileRole{PersonFileRoleConversationMember},
		Directions: []PersonFileDirection{PersonFileGroup},
	}, defaultScope.Files[4].PersonProvenance)
	assertions.Equal(int64(5), defaultScope.TotalCount)

	unrestricted, err := engine.SearchFiles(context.Background(), FileSearchRequest{
		Person: &PersonFileScope{
			ParticipantIDs: []int64{person, alias},
			Directions:     []PersonFileDirection{PersonFileFromPerson, PersonFileToPerson, PersonFileGroup},
		},
		Sort: SortSpec{Field: sortFieldOccurredAt, Direction: "desc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(unrestricted.Files, 5)
	assertions.Equal([]int64{905, 901, 902, 903, 904}, []int64{
		unrestricted.Files[0].ID, unrestricted.Files[1].ID,
		unrestricted.Files[2].ID, unrestricted.Files[3].ID, unrestricted.Files[4].ID,
	})
	assertions.Equal(int64(5), unrestricted.TotalCount)

	deletedOnly, err := engine.SearchFiles(context.Background(), FileSearchRequest{
		Explore: ExploreRequest{Context: Context{Deletion: DeletionDeleted}},
		Person:  &PersonFileScope{ParticipantIDs: []int64{person, alias}, Directions: []PersonFileDirection{PersonFileFromPerson}},
		Sort:    SortSpec{Field: sortFieldOccurredAt, Direction: "desc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(deletedOnly.Files, 1)
	assertions.Equal(int64(905), deletedOnly.Files[0].ID)
	assertions.Equal(int64(1), deletedOnly.TotalCount)
}

func TestSearchFilesPersonScopeDirectChatSenderUsesWholeCluster(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	person := b.AddParticipant("person@example.com", "example.com", "Person")
	alias := b.AddParticipant("person.alias@example.net", "example.net", "Person Alias")
	other := b.AddParticipant("other@example.org", "example.org", "Other")
	base := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	nullSender := b.AddMessage(MessageOpt{
		SourceID: source, ConversationID: 801, ConversationType: "direct_chat",
		Subject: "null sender", SentAt: base,
	})
	b.AddFrom(nullSender, alias, "Person Alias")
	b.AddConversationParticipant(801, person)
	b.AddAttachmentWithMIME(1001, nullSender, 10, "null-sender.txt", "text/plain")

	linkedSender := b.AddMessage(MessageOpt{
		SourceID: source, ConversationID: 802, ConversationType: "direct_chat",
		Subject: "linked sender", SentAt: base.Add(-time.Hour), SenderID: &alias,
	})
	b.AddConversationParticipant(802, person)
	b.AddAttachmentWithMIME(1002, linkedSender, 20, "linked-sender.txt", "text/plain")

	otherSender := b.AddMessage(MessageOpt{
		SourceID: source, ConversationID: 803, ConversationType: "direct_chat",
		Subject: "other sender", SentAt: base.Add(-2 * time.Hour), SenderID: &other,
	})
	b.AddConversationParticipant(803, person)
	b.AddAttachmentWithMIME(1003, otherSender, 30, "to-person.txt", "text/plain")

	authorless := b.AddMessage(MessageOpt{
		SourceID: source, ConversationID: 804, ConversationType: "direct_chat",
		Subject: "authorless", SentAt: base.Add(-3 * time.Hour), IsFromMe: false,
	})
	b.AddConversationParticipant(804, person)
	b.AddAttachmentWithMIME(1004, authorless, 40, "authorless.txt", "text/plain")

	engine := b.BuildEngine()
	search := func(direction PersonFileDirection) *FileSearchResponse {
		result, err := engine.SearchFiles(context.Background(), FileSearchRequest{
			Explore: ExploreRequest{Context: Context{Deletion: DeletionActive}},
			Person:  &PersonFileScope{ParticipantIDs: []int64{person, alias}, Directions: []PersonFileDirection{direction}},
			Sort:    SortSpec{Field: sortFieldOccurredAt, Direction: "desc"}, Page: PageSpec{Limit: 10},
		})
		requirements.NoError(err)
		return result
	}

	fromPerson := search(PersonFileFromPerson)
	requirements.Len(fromPerson.Files, 2)
	assertions.Equal([]string{"null-sender.txt", "linked-sender.txt"}, []string{
		fromPerson.Files[0].Filename, fromPerson.Files[1].Filename,
	})
	for _, file := range fromPerson.Files {
		requirements.NotNil(file.PersonProvenance)
		assertions.Equal([]PersonFileDirection{PersonFileFromPerson}, file.PersonProvenance.Directions)
	}

	toPerson := search(PersonFileToPerson)
	requirements.Len(toPerson.Files, 1)
	assertions.Equal("to-person.txt", toPerson.Files[0].Filename)
	requirements.NotNil(toPerson.Files[0].PersonProvenance)
	assertions.Equal([]PersonFileDirection{PersonFileToPerson}, toPerson.Files[0].PersonProvenance.Directions)

	unclassified, err := engine.SearchFiles(context.Background(), FileSearchRequest{
		Explore: ExploreRequest{Context: Context{Deletion: DeletionActive}},
		Person: &PersonFileScope{
			ParticipantIDs: []int64{person, alias},
			Directions: []PersonFileDirection{
				PersonFileFromPerson, PersonFileToPerson, PersonFileGroup,
			},
			IncludeUnclassifiedRosterRows: true,
		},
		Sort: SortSpec{Field: sortFieldOccurredAt, Direction: "desc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(unclassified.Files, 4)
	assertions.Equal([]string{"null-sender.txt", "linked-sender.txt", "to-person.txt", "authorless.txt"}, []string{
		unclassified.Files[0].Filename, unclassified.Files[1].Filename,
		unclassified.Files[2].Filename, unclassified.Files[3].Filename,
	})
	assertions.Equal(&PersonFileProvenance{
		ParticipantIDs: []int64{person},
		Roles:          []PersonFileRole{PersonFileRoleConversationMember},
		Directions:     []PersonFileDirection{PersonFileGroup},
	}, unclassified.Files[3].PersonProvenance)
}

func TestSearchFilesPersonScopeValidation(t *testing.T) {
	b := NewTestDataBuilder(t)
	b.AddSource("archive@example.com")
	engine := b.BuildEngine()

	tests := []struct {
		name  string
		scope PersonFileScope
	}{
		{name: "missing participant IDs", scope: PersonFileScope{Directions: []PersonFileDirection{PersonFileFromPerson}}},
		{name: "non-positive participant ID", scope: PersonFileScope{ParticipantIDs: []int64{0}, Directions: []PersonFileDirection{PersonFileFromPerson}}},
		{name: "missing directions", scope: PersonFileScope{ParticipantIDs: []int64{1}}},
		{name: "unknown direction", scope: PersonFileScope{ParticipantIDs: []int64{1}, Directions: []PersonFileDirection{"sideways"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := engine.SearchFiles(context.Background(), FileSearchRequest{Person: &test.scope})
			require.ErrorIs(t, err, ErrInvalidExploreRequest)
		})
	}
}

func TestSearchFilesFlattensSnippetMarkupInContainingTitle(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("meeting@example.com", "meeting")
	when := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	message := b.AddMessage(MessageOpt{
		SourceID: source, Subject: "", Snippet: "### Meeting notes\n- Action item", SentAt: when,
	})
	b.AddAttachmentWithMIME(61, message, 1024, "notes.pdf", "application/pdf")

	result, err := b.BuildEngine().SearchFiles(context.Background(), FileSearchRequest{
		Sort: SortSpec{Field: "filename", Direction: "asc"}, Page: PageSpec{Limit: 25},
	})
	requirements.NoError(err)
	requirements.Len(result.Files, 1)
	assertions.Equal("Meeting notes Action item", result.Files[0].ContainingTitle)
}

func TestSearchFilesPreservesSubjectContainingTitleWithMarkdownLikeCharacters(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	when := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	message := b.AddMessage(MessageOpt{
		SourceID: source, Subject: "Re: 2 ** 3 == 8?", Snippet: "### Meeting notes\n- Action item", SentAt: when,
	})
	b.AddAttachmentWithMIME(62, message, 1024, "notes.pdf", "application/pdf")

	result, err := b.BuildEngine().SearchFiles(context.Background(), FileSearchRequest{
		Sort: SortSpec{Field: "filename", Direction: "asc"}, Page: PageSpec{Limit: 25},
	})
	requirements.NoError(err)
	requirements.Len(result.Files, 1)
	assertions.Equal("Re: 2 ** 3 == 8?", result.Files[0].ContainingTitle)
}

func TestSearchFilesUsesStableDateNameAndSizeSorts(t *testing.T) {
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	when := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	message := b.AddMessage(MessageOpt{SourceID: source, Subject: "Duplicates", SentAt: when})
	b.AddAttachmentWithMIME(51, message, 20, "same.png", "image/png")
	b.AddAttachmentWithMIME(52, message, 10, "same.png", "image/png")
	b.AddAttachmentWithMIME(53, message, 10, "alpha.png", "image/png")
	engine := b.BuildEngine()

	tests := []struct {
		name      string
		sort      SortSpec
		wantFirst int64
	}{
		{name: "date", sort: SortSpec{Field: sortFieldOccurredAt, Direction: "desc"}, wantFirst: 51},
		{name: "name", sort: SortSpec{Field: "filename", Direction: "asc"}, wantFirst: 53},
		{name: "size", sort: SortSpec{Field: "size", Direction: "asc"}, wantFirst: 53},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			result, err := engine.SearchFiles(context.Background(), FileSearchRequest{
				Sort: test.sort, Page: PageSpec{Limit: 1},
			})
			requirements.NoError(err)
			requirements.Len(result.Files, 1)
			assertions.Equal(test.wantFirst, result.Files[0].ID)
			assertions.Equal(int64(3), result.TotalCount)

			second, secondErr := engine.SearchFiles(context.Background(), FileSearchRequest{
				Sort: test.sort, Page: PageSpec{Limit: 1, Offset: 1},
			})
			requirements.NoError(secondErr)
			requirements.Len(second.Files, 1)
			assertions.NotEqual(result.Files[0].Key, second.Files[0].Key)
		})
	}
}

func TestSearchFilesNamesUnavailableCache(t *testing.T) {
	engine, err := NewDuckDBEngine("", "", nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, engine.Close()) })

	_, err = engine.SearchFiles(context.Background(), FileSearchRequest{})
	var unavailable *CacheUnavailableError
	require.ErrorAs(t, err, &unavailable)
	assert.Equal(t, CacheAbsent, unavailable.Readiness)
}

func TestGroupFilesUsesExactFilteredFilePopulation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	sourceA := b.AddSourceWithType("archive-a@example.com", "gmail")
	sourceB := b.AddSourceWithType("archive-b@example.com", "imap")
	inside := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	first := b.AddMessage(MessageOpt{SourceID: sourceA, Subject: "First", SentAt: inside})
	b.AddAttachmentWithMIME(101, first, 100, "invoice-one.pdf", "application/pdf")
	b.AddAttachmentWithMIME(102, first, 25, "invoice-image.png", "image/png")
	second := b.AddMessage(MessageOpt{SourceID: sourceA, Subject: "Second", SentAt: inside.Add(-time.Hour)})
	b.AddAttachmentWithMIME(103, second, 200, "invoice-two.pdf", "application/pdf")
	third := b.AddMessage(MessageOpt{SourceID: sourceB, Subject: "Other source", SentAt: inside})
	b.AddAttachmentWithMIME(104, third, 400, "invoice-three.pdf", "application/pdf")

	engine := b.BuildEngine()
	request := FileGroupRequest{
		Explore:       ExploreRequest{Context: Context{SourceIDs: []int64{sourceA}}},
		FilenameQuery: "invoice", MIMEFamilies: []FileMIMEFamily{FileMIMEPDF},
		Dimension: "source", Sort: SortSpec{Field: "count", Direction: "desc"},
		Page: PageSpec{Limit: 10},
	}
	grouped, err := engine.GroupFiles(context.Background(), request)
	requirements.NoError(err)
	requirements.Equal(int64(1), grouped.TotalCount)
	requirements.Len(grouped.Rows, 1)
	assertions.Equal("1", grouped.Rows[0].Key)
	assertions.Equal(int64(2), grouped.Rows[0].Count)
	assertions.Equal(int64(300), grouped.Rows[0].EstimatedBytes)
	assertions.NotEmpty(grouped.CacheRevision)

	files, err := engine.SearchFiles(context.Background(), FileSearchRequest{
		Explore: request.Explore, FilenameQuery: request.FilenameQuery, MIMEFamilies: request.MIMEFamilies,
		Sort: SortSpec{Field: sortFieldOccurredAt, Direction: "desc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	assertions.Equal(files.TotalCount, grouped.Rows[0].Count, "group count must equal filtered Files rows")
}

// TestGroupFilesMessageTypeCollapsesLegacyRowsIntoEmail pins the file-group
// side of the legacy-row rule: attachments on rows imported before
// message_type existed (blank value) are email files (see
// duckDBMessageTypeCondition and store.IsEmailMessageType), so the
// message-type dimension must fold them into the 'email' group instead of
// emitting a separate unlabeled row — and the 'email' file filter that group
// row drills into must reproduce the group row's count.
func TestGroupFilesMessageTypeCollapsesLegacyRowsIntoEmail(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	b.AddSource("archive@example.com")
	base := time.Date(2024, 3, 1, 9, 0, 0, 0, time.UTC)
	typed := b.AddMessage(MessageOpt{Subject: "typed email", MessageType: messageTypeEmail, SentAt: base})
	b.AddAttachmentWithMIME(401, typed, 100, "typed.pdf", "application/pdf")
	legacy := b.AddMessage(MessageOpt{Subject: "legacy email", LegacyEmptyMessageType: true, SentAt: base.Add(time.Hour)})
	b.AddAttachmentWithMIME(402, legacy, 30, "legacy.pdf", "application/pdf")
	calendar := b.AddMessage(MessageOpt{Subject: "calendar", MessageType: messageTypeCalendar, SentAt: base.Add(2 * time.Hour)})
	b.AddAttachmentWithMIME(403, calendar, 7, "invite.ics", "text/calendar")
	engine := b.BuildEngine()

	result, err := engine.GroupFiles(context.Background(), FileGroupRequest{
		Dimension: messageTypeDimension,
		Sort:      SortSpec{Field: "count", Direction: "desc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(result.Rows, 2, "legacy blank rows must fold into 'email', not form a third group")
	assertions.Equal(int64(2), result.TotalCount)
	assertions.Equal(messageTypeEmail, result.Rows[0].Key)
	assertions.Equal(messageTypeEmail, result.Rows[0].Label)
	assertions.Equal(int64(2), result.Rows[0].Count, "'email' group must include the legacy row's file")
	assertions.Equal(int64(130), result.Rows[0].EstimatedBytes)
	assertions.Equal(messageTypeCalendar, result.Rows[1].Key)
	assertions.Equal(int64(1), result.Rows[1].Count)

	drilled, err := engine.SearchFiles(context.Background(), FileSearchRequest{
		Explore: ExploreRequest{Context: Context{MessageTypes: []string{messageTypeEmail}}},
		Sort:    SortSpec{Field: sortFieldOccurredAt, Direction: "desc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	assertions.Equal(result.Rows[0].Count, drilled.TotalCount, "drill-down file filter must reproduce the group count")
}

func TestGroupFilesDeduplicatesParticipantAndDomainMembershipPerFile(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	alice := b.AddParticipant("alice@example.com", "example.com", "Alice Example")
	bob := b.AddParticipant("bob@example.com", "example.com", "Bob Example")
	message := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 77, SenderID: &alice, Subject: "Participants"})
	b.AddFrom(message, alice, "Alice duplicate sender")
	b.AddTo(message, alice, "Alice duplicate recipient")
	b.AddTo(message, bob, "Bob")
	b.AddConversationParticipant(77, alice)
	b.AddConversationParticipant(77, bob)
	b.AddAttachmentWithMIME(201, message, 125, "people.pdf", "application/pdf")

	engine := b.BuildEngine()
	participants, err := engine.GroupFiles(context.Background(), FileGroupRequest{
		Dimension: "participant", Sort: SortSpec{Field: "key", Direction: "asc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(participants.Rows, 2)
	assertions.Equal([]ExploreGroupRow{
		{Key: "1", Label: "Alice Example", Count: 1, EstimatedBytes: 125, LatestAt: participants.Rows[0].LatestAt},
		{Key: "2", Label: "Bob Example", Count: 1, EstimatedBytes: 125, LatestAt: participants.Rows[1].LatestAt},
	}, participants.Rows)

	domains, err := engine.GroupFiles(context.Background(), FileGroupRequest{
		Dimension: "domain", Sort: SortSpec{Field: "key", Direction: "asc"}, Page: PageSpec{Limit: 1},
	})
	requirements.NoError(err)
	requirements.Len(domains.Rows, 1)
	assertions.Equal(int64(1), domains.TotalCount)
	assertions.Equal("example.com", domains.Rows[0].Key)
	assertions.Equal(int64(1), domains.Rows[0].Count)
	assertions.Equal(int64(125), domains.Rows[0].EstimatedBytes)
}

// TestGroupFilesLabelsRosterOnlyParticipants pins label completeness for a
// participant whose only archive activity is conversation-roster membership
// on a non-chat message: files still attribute to them (matching the legacy
// direct-plus-roster membership), but the relationship_people dataset has no
// row for them — the label must come from the base participant record, not
// the "Unknown person #" fallback.
func TestGroupFilesLabelsRosterOnlyParticipants(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	alice := b.AddParticipant("alice@example.com", "example.com", "Alice Example")
	carol := b.AddParticipant("carol@example.com", "example.com", "Carol Example")
	message := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 88, SenderID: &alice, Subject: "Roster"})
	b.AddFrom(message, alice, "Alice Example")
	b.AddConversationParticipant(88, alice)
	b.AddConversationParticipant(88, carol)
	b.AddAttachmentWithMIME(401, message, 55, "roster.pdf", "application/pdf")

	result, err := b.BuildEngine().GroupFiles(context.Background(), FileGroupRequest{
		Dimension: "participant", Sort: SortSpec{Field: "key", Direction: "asc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(result.Rows, 2)
	assertions.Equal("Alice Example", result.Rows[0].Label)
	assertions.Equal("Carol Example", result.Rows[1].Label,
		"a roster-only participant must keep their real name")
	assertions.Equal(int64(1), result.Rows[1].Count)
}

// linkedParticipantFilesFixture builds an attachment archive where alice and
// her work alias are one linked identity cluster (canonical = alice, the
// smallest member ID): one file's message lists BOTH aliases, one file's
// message lists only the alias, and one file involves only the unlinked bob.
func linkedParticipantFilesFixture(t *testing.T) (b *TestDataBuilder, alice, alias int64) {
	t.Helper()
	b = NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	alice = b.AddParticipant("alice@example.com", "example.com", "Alice Example")
	alias = b.AddParticipant("alice@work.example", "work.example", "Alice (Work)")
	bob := b.AddParticipant("bob@example.com", "example.com", "Bob Example")
	b.LinkCluster(alice, alias)

	both := b.AddMessage(MessageOpt{SourceID: source, Subject: "Both aliases",
		SentAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)})
	b.AddTo(both, alice, "")
	b.AddCc(both, alias, "")
	b.AddAttachmentWithMIME(301, both, 100, "both.pdf", "application/pdf")
	aliasOnly := b.AddMessage(MessageOpt{SourceID: source, Subject: "Alias only",
		SentAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)})
	b.AddTo(aliasOnly, alias, "")
	b.AddAttachmentWithMIME(302, aliasOnly, 30, "alias.pdf", "application/pdf")
	bobOnly := b.AddMessage(MessageOpt{SourceID: source, Subject: "Bob only",
		SentAt: time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)})
	b.AddTo(bobOnly, bob, "")
	b.AddAttachmentWithMIME(303, bobOnly, 7, "bob.pdf", "application/pdf")
	return b, alice, alias
}

func TestGroupFilesMergesLinkedParticipantIdentities(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b, _, _ := linkedParticipantFilesFixture(t)

	result, err := b.BuildEngine().GroupFiles(context.Background(), FileGroupRequest{
		Dimension: "participant", Sort: SortSpec{Field: "key", Direction: "asc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(result.Rows, 2, "linked aliases must merge into one canonical row")
	// The file whose message lists both aliases counts ONCE; the alias-only
	// file merges into the canonical row; the label follows the cluster
	// best-name policy (smallest named member), not the latest alias's name.
	assertions.Equal([]ExploreGroupRow{
		{Key: "1", Label: "Alice Example", Count: 2, EstimatedBytes: 130, LatestAt: result.Rows[0].LatestAt},
		{Key: "3", Label: "Bob Example", Count: 1, EstimatedBytes: 7, LatestAt: result.Rows[1].LatestAt},
	}, result.Rows)
}

func TestSearchFilesParticipantFilterMatchesLinkedAliases(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b, alice, alias := linkedParticipantFilesFixture(t)
	engine := b.BuildEngine()

	byCanonical, err := engine.SearchFiles(context.Background(), FileSearchRequest{
		Explore: ExploreRequest{Context: Context{ParticipantIDs: []int64{alice}}},
		Sort:    SortSpec{Field: sortFieldOccurredAt, Direction: "desc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(byCanonical.Files, 2, "canonical-ID filter must include alias-owned files")
	assertions.Equal(int64(302), byCanonical.Files[0].ID, "alias-only file")
	assertions.Equal(int64(301), byCanonical.Files[1].ID)

	byAlias, err := engine.SearchFiles(context.Background(), FileSearchRequest{
		Explore: ExploreRequest{Context: Context{ParticipantIDs: []int64{alias}}},
		Sort:    SortSpec{Field: sortFieldOccurredAt, Direction: "desc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	assertions.Len(byAlias.Files, 2, "a member ID widens to its whole cluster")
}

// TestSearchFilesAdditionalParticipantGroupIntersects proves the files
// endpoint honors a conjunctive drill-down (A∩B) rather than replacing the
// base participant filter with the drilled-into group.
func TestSearchFilesAdditionalParticipantGroupIntersects(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.com")
	alice := b.AddParticipant("alice@example.com", "example.com", "Alice")
	bob := b.AddParticipant("bob@example.com", "example.com", "Bob")

	onlyAlice := b.AddMessage(MessageOpt{SourceID: source,
		SentAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)})
	b.AddTo(onlyAlice, alice, "")
	b.AddAttachmentWithMIME(401, onlyAlice, 10, "alice.pdf", "application/pdf")
	onlyBob := b.AddMessage(MessageOpt{SourceID: source,
		SentAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)})
	b.AddTo(onlyBob, bob, "")
	b.AddAttachmentWithMIME(402, onlyBob, 10, "bob.pdf", "application/pdf")
	both := b.AddMessage(MessageOpt{SourceID: source,
		SentAt: time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)})
	b.AddTo(both, alice, "")
	b.AddCc(both, bob, "")
	b.AddAttachmentWithMIME(403, both, 10, "both.pdf", "application/pdf")
	engine := b.BuildEngine()

	result, err := engine.SearchFiles(context.Background(), FileSearchRequest{
		Explore: ExploreRequest{Context: Context{
			ParticipantIDs: []int64{alice}, AdditionalParticipantGroups: [][]int64{{bob}},
		}},
		Sort: SortSpec{Field: sortFieldOccurredAt, Direction: "desc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(result.Files, 1, "only the file on the entry involving both Alice and Bob must match")
	assertions.Equal(int64(403), result.Files[0].ID)
}

func TestGroupFilesNamesUnavailableCache(t *testing.T) {
	engine, err := NewDuckDBEngine("", "", nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, engine.Close()) })

	_, err = engine.GroupFiles(context.Background(), FileGroupRequest{})
	var unavailable *CacheUnavailableError
	require.ErrorAs(t, err, &unavailable)
	assert.Equal(t, CacheAbsent, unavailable.Readiness)
}
