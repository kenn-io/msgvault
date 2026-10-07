package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const personIdentitySanitizedValue = "@carol\x1b[31m:example.org"

// newPersonIdentityFixture creates one durable person whose cluster holds a
// column email, an identifier-only email, a phone number, and two chat
// identifiers, beside an unrelated participant and a curated-only contact point.
func newPersonIdentityFixture(t *testing.T) (draftReplyFixture, int64) {
	t.Helper()
	require := require.New(t)
	fixture := newDraftReplyFixture(t)
	st := fixture.store
	columnID, err := st.EnsureParticipant("carol@example.com", "Carol", "example.com")
	require.NoError(err)
	identifierID, err := st.EnsureParticipantByIdentifier("email", "carol.alt@example.com", "")
	require.NoError(err)
	phoneID, err := st.EnsurePhoneParticipantContext(t.Context(), "+15555550100", "")
	require.NoError(err)
	chatID, err := st.EnsureParticipantByIdentifier("imessage", "carol-chat@example.org", "")
	require.NoError(err)
	matrixID, err := st.EnsureParticipantByIdentifier("matrix", personIdentitySanitizedValue, "")
	require.NoError(err)
	for _, member := range []int64{identifierID, phoneID, chatID, matrixID} {
		_, err = st.LinkParticipants(columnID, member)
		require.NoError(err)
	}
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), columnID)
	require.NoError(err)
	require.ElementsMatch([]int64{columnID, identifierID, phoneID, chatID, matrixID}, person.ParticipantIDs)
	_, err = st.EnsureParticipant("unrelated@example.com", "Unrelated", "example.com")
	require.NoError(err)
	_, err = st.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "curated-only@example.com",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	return fixture, person.ID
}

// runPersonIdentitiesCommand runs `person identities` against a daemon API
// served from the fixture's store.
func runPersonIdentitiesCommand(t *testing.T, fixture draftReplyFixture, args ...string) (string, error) {
	t.Helper()
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{HomeDir: t.TempDir()},
		Store:  fixture.grantedAdapter(),
		Logger: slog.New(slog.DiscardHandler),
	}).Router())
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	root := &cobra.Command{Use: "msgvault", SilenceErrors: true, SilenceUsage: true}
	person := &cobra.Command{Use: personValue}
	person.AddCommand(newPersonIdentitiesCommand())
	root.AddCommand(person)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(append([]string{personValue, "identities"}, args...))
	err := root.ExecuteContext(ctx)
	return output.String(), err
}

func listPersonIdentitiesJSON(t *testing.T, fixture draftReplyFixture, personID int64) []api.PersonIdentity {
	t.Helper()
	output, err := runPersonIdentitiesCommand(t, fixture, strconv.FormatInt(personID, 10), "--json")
	require.NoError(t, err)
	var response api.PersonIdentitiesResponse
	require.NoError(t, json.Unmarshal([]byte(output), &response))
	require.Equal(t, personID, response.PersonID)
	return response.Identities
}

func TestPersonIdentitiesListsArchivedIdentities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture, personID := newPersonIdentityFixture(t)

	assert.ElementsMatch([]api.PersonIdentity{
		{Kind: "email", Value: "carol@example.com", Supported: true},
		{Kind: "email", Value: "carol.alt@example.com", Supported: true},
		{Kind: "phone", Value: "+15555550100"},
		{Kind: "imessage", Value: "carol-chat@example.org"},
		{Kind: "matrix", Value: personIdentitySanitizedValue},
	}, listPersonIdentitiesJSON(t, fixture, personID))

	output, err := runPersonIdentitiesCommand(t, fixture, strconv.FormatInt(personID, 10))
	require.NoError(err)
	var rows [][]string
	for line := range strings.Lines(output) {
		rows = append(rows, strings.Fields(line))
	}
	require.NotEmpty(rows)
	assert.Equal([]string{"KIND", "VALUE", "DRAFTS"}, rows[0])
	assert.ElementsMatch([][]string{
		{"email", "carol@example.com", "supported"},
		{"email", "carol.alt@example.com", "supported"},
		{"phone", "+15555550100", "unsupported"},
		{"imessage", "carol-chat@example.org", "unsupported"},
		{"matrix", "@carol:example.org", "unsupported"},
	}, rows[1:])
	assert.NotContains(output, "\x1b")
}

func TestPersonIdentitiesRejectsUnknownPerson(t *testing.T) {
	fixture := newDraftReplyFixture(t)
	_, err := runPersonIdentitiesCommand(t, fixture, "999999")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Person profile not found")
}

func TestPersonIdentitiesKeepsCaseDistinctIdentifiers(t *testing.T) {
	require := require.New(t)
	fixture, personID := newPersonIdentityFixture(t)
	st := fixture.store
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	upperID, err := st.EnsureParticipantByIdentifier("matrix", "@Carol:example.org", "")
	require.NoError(err)
	lowerID, err := st.EnsureParticipantByIdentifier("matrix", "@carol:example.org", "")
	require.NoError(err)
	require.NotEqual(upperID, lowerID)
	for _, member := range []int64{upperID, lowerID} {
		_, err = st.LinkParticipants(person.ParticipantIDs[0], member)
		require.NoError(err)
	}
	person, err = st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	require.Subset(person.ParticipantIDs, []int64{upperID, lowerID})

	identities, err := fixture.grantedAdapter().ListPersonIdentitiesContext(t.Context(), personID)
	require.NoError(err)
	assert.Subset(t, identities, []api.PersonIdentity{
		{Kind: "matrix", Value: "@Carol:example.org"},
		{Kind: "matrix", Value: "@carol:example.org"},
	})
}

func TestPersonIdentitiesSaysWhenNothingIsArchived(t *testing.T) {
	require := require.New(t)
	fixture := newDraftReplyFixture(t)
	participantID, err := fixture.store.EnsureParticipant("nobody@example.com", "", "example.com")
	require.NoError(err)
	person, _, err := fixture.store.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(err)
	// A participant with no email, phone, or identifier, such as a display-name-only sender.
	_, err = fixture.store.DB().Exec(fixture.store.Rebind("UPDATE participants SET email_address = NULL WHERE id = ?"), participantID)
	require.NoError(err)
	_, err = fixture.store.DB().Exec(fixture.store.Rebind("DELETE FROM participant_identifiers WHERE participant_id = ?"), participantID)
	require.NoError(err)

	output, err := runPersonIdentitiesCommand(t, fixture, strconv.FormatInt(person.ID, 10))
	require.NoError(err)
	assert.Equal(t, fmt.Sprintf("Person %d has no archived identities\n", person.ID), output)
	assert.Empty(t, listPersonIdentitiesJSON(t, fixture, person.ID))
}

// An imported "first last"@example.com is stored without its quotes, so the
// listing must restore them for the value to work as --to.
func TestPersonIdentitiesListImportedQuotedMailbox(t *testing.T) {
	for _, mailbox := range []string{`"first last"@example.com`, `" alice"@example.com`} {
		t.Run(mailbox, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			fixture := newDraftReplyFixture(t)
			raw := []byte("From: Quoted <" + mailbox + ">\r\n" +
				"To: " + testutil.IMAPTestUsername + "\r\n" +
				"Subject: Quoted\r\n" +
				"Message-ID: <quoted@example.com>\r\n\r\n" +
				"Body\r\n")
			require.NoError(importer.IngestRawMessage(t.Context(), fixture.store, fixture.source.ID,
				testutil.IMAPTestUsername, "", nil, "INBOX|quoted", "quoted-hash", raw, time.Now(),
				slog.New(slog.DiscardHandler)))
			participantsBefore := countParticipants(t, fixture.store)
			participantID, err := fixture.store.EnsureParticipant(strings.ReplaceAll(mailbox, `"`, ""), "", "example.com")
			require.NoError(err)
			require.Equal(participantsBefore, countParticipants(t, fixture.store), "import must store the mailbox unquoted")
			person, _, err := fixture.store.CreatePersonFromParticipantContext(t.Context(), participantID)
			require.NoError(err)

			identities := listPersonIdentitiesJSON(t, fixture, person.ID)
			require.Equal([]api.PersonIdentity{{Kind: "email", Value: mailbox, Supported: true}}, identities)

			var events []api.CLIRunEvent
			err = fixture.grantedAdapter().runCLIComposeDraft(t.Context(), api.CLIRunRequest{Args: []string{
				api.CLIRunDraftComposeCommand, "--source-id", strconv.FormatInt(fixture.source.ID, 10),
				"--from", testutil.IMAPTestUsername, "--to", identities[0].Value, "--body", "x", "--json",
			}}, func(event api.CLIRunEvent) error {
				events = append(events, event)
				return nil
			})
			require.NoError(err)
			require.Len(events, 1)
			var result draftReplyOutput
			require.NoError(json.Unmarshal([]byte(events[0].Data), &result))
			assert.Equal(draftReplyStatusCreated, result.Status)
			storedRaw, err := fixture.store.GetMessageRaw(result.MessageID)
			require.NoError(err)
			assert.Contains(string(storedRaw), "To: <"+mailbox+">")
		})
	}
}

func countParticipants(t *testing.T, st *store.Store) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRow("SELECT COUNT(*) FROM participants").Scan(&count))
	return count
}
