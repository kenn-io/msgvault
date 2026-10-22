package meetingimport

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNativeMeetingUnchangedExistingLibraryRowAdoptsSilentDecision(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	req := validImportRequest(t)
	normalized, err := req.Normalize()
	require.NoError(err)
	snap, err := BuildSnapshot(normalized)
	require.NoError(err)
	source, err := st.GetOrCreateSource(SourceType, snap.SourceIdentifier)
	require.NoError(err)
	var organizer *meetingarchive.Person
	if snap.Organizer != nil {
		person := archivePerson(snap.SourceIdentifier, *snap.Organizer)
		organizer = &person
	}
	var attendees []meetingarchive.Person
	for _, person := range snap.Attendees {
		attendees = append(attendees, archivePerson(snap.SourceIdentifier, person))
	}
	// The shared native archiver is also used outside explicit import runs.
	legacy, err := meetingarchive.New(st).Upsert(t.Context(), meetingarchive.Snapshot{SourceID: source.ID, AccountEmail: snap.AccountEmail, SourceMessageID: snap.SourceMessageID, SourceConversationID: snap.SourceMessageID, Title: snap.Title, StartedAt: snap.StartedAt, Body: snap.Body, Snippet: snap.Snippet, Metadata: snap.Metadata, Raw: snap.Raw, RawFormat: RawFormat, Organizer: organizer, Attendees: attendees}, meetingarchive.UpsertOptions{})
	require.NoError(err)
	var before int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_message_decisions`).Scan(&before))
	require.Zero(before)
	result, err := NewImporter(st, Hooks{}).Import(t.Context(), req)
	require.NoError(err)
	assert.Equal(legacy.MessageID, result.MessageID)
	latest, err := st.GetLatestSync(source.ID)
	require.NoError(err)
	assert.Zero(latest.MessagesUpdated, "the native unchanged fast path must remain unchanged")
	var decisions int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_message_decisions WHERE notified=FALSE AND import_run_id IS NULL AND archived_at IS NULL`).Scan(&decisions))
	assert.Equal(1, decisions, "existing native fast paths preserve silent identity without fabricated historical run data")
}
