package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCardDAVPublicationStateSourceUsesOneReadSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "publication-state.db")
	reader, err := OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = reader.Close() })
	require.NoError(reader.InitSchema())
	participantID, err := reader.EnsureParticipant("alice@example.test", "Alice", "example.test")
	require.NoError(err)
	person, _, err := reader.CreatePersonFromParticipant(participantID)
	require.NoError(err)
	allowed := true
	_, books, err := reader.ReplaceCardDAVDiscoveryContext(t.Context(), CardDAVDiscoveryInput{
		BaseURL: "https://carddav.example.test", Username: "alice",
		PrincipalURL: "https://carddav.example.test/principal/",
		HomeURL:      "https://carddav.example.test/books/",
		Books: []CardDAVDiscoveredBook{{
			CanonicalURL: "https://carddav.example.test/books/personal/", DisplayName: "Personal",
			CanCreate: &allowed,
		}},
	})
	require.NoError(err)
	require.Len(books, 1)

	writer, err := OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = writer.Close() })
	reader.cardDAVPublicationStateReadHook = func() {
		reader.cardDAVPublicationStateReadHook = nil
		require.NoError(writer.DeletePersonContext(t.Context(), person.ID, person.Revision))
	}

	source, err := reader.GetCardDAVPublicationStateSourceContext(t.Context(), person.ID)
	require.NoError(err)
	assert.Equal(person.ID, source.PersonID)
	assert.Equal(books[0].ID, source.ProspectiveBookID,
		"the remaining reads must come from the same pre-delete snapshot")
	_, err = reader.GetPersonContext(t.Context(), person.ID)
	assert.ErrorIs(err, ErrPersonNotFound)
}
