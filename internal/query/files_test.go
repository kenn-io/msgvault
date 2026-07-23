package query

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		{name: "date", sort: SortSpec{Field: "occurred_at", Direction: "desc"}, wantFirst: 51},
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
		Sort: SortSpec{Field: "occurred_at", Direction: "desc"}, Page: PageSpec{Limit: 10},
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
		Sort:    SortSpec{Field: "occurred_at", Direction: "desc"}, Page: PageSpec{Limit: 10},
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
		Sort:    SortSpec{Field: "occurred_at", Direction: "desc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	requirements.Len(byCanonical.Files, 2, "canonical-ID filter must include alias-owned files")
	assertions.Equal(int64(302), byCanonical.Files[0].ID, "alias-only file")
	assertions.Equal(int64(301), byCanonical.Files[1].ID)

	byAlias, err := engine.SearchFiles(context.Background(), FileSearchRequest{
		Explore: ExploreRequest{Context: Context{ParticipantIDs: []int64{alias}}},
		Sort:    SortSpec{Field: "occurred_at", Direction: "desc"}, Page: PageSpec{Limit: 10},
	})
	requirements.NoError(err)
	assertions.Len(byAlias.Files, 2, "a member ID widens to its whole cluster")
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
