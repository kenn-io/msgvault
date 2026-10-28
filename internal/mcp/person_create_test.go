package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type creatingPeopleBackend struct {
	recordingPeopleBackend

	store *store.Store
	err   error
}

func (b *creatingPeopleBackend) CreatePerson(ctx context.Context, input store.PersonCreateInput) (*store.Person, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.store.CreateStandalonePersonContext(ctx, input)
}

func personCreateToolOptions(backend *creatingPeopleBackend) ServeOptions {
	opts := peopleToolOptions(backend)
	opts.PersonCreator = backend
	return opts
}

func TestCreatePersonToolGatingAndCreation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	backend := &creatingPeopleBackend{store: testutil.NewTestStore(t)}
	opts := personCreateToolOptions(backend)
	readOnly := toolsByName(t, rawListTools(t, opts, false))
	assert.NotContains(readOnly, "create_person")
	noProfile := opts
	noProfile.AllowProfileWrites = false
	assert.NotContains(toolsByName(t, rawListTools(t, noProfile, true)), "create_person")
	olderDaemon := peopleToolOptions(backend)
	olderTools := toolsByName(t, rawListTools(t, olderDaemon, true))
	assert.NotContains(olderTools, "create_person")
	assert.Contains(olderTools, "promote_person")
	writable := toolsByName(t, rawListTools(t, opts, true))
	require.Contains(writable, "create_person")
	assert.Equal(false, toolReadOnlyHint(t, writable["create_person"]))
	result := rawCallTool(t, opts, "create_person", map[string]any{
		"name": "Alex Example", "emails": []any{map[string]any{"value": "alex@example.com"}},
		"org": "Example Company", "note": "Met at a conference",
	})
	require.NotEqual(true, result["isError"], "%#v", result)
	people, err := backend.store.ListPersonsContext(t.Context())
	require.NoError(err)
	require.Len(people, 1)
	assert.Empty(people[0].ParticipantIDs)
	notes, err := backend.store.ListPersonAttributeValuesContext(t.Context(), people[0].ID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugNotes})
	require.NoError(err)
	require.Len(notes, 1)
	assert.Equal(store.ProvenanceEnrichment, notes[0].Source)
	duplicate := rawCallTool(t, opts, "create_person", map[string]any{"name": "Alex Duplicate", "emails": []any{map[string]any{"value": "ALEX@example.com"}}})
	assert.Equal(true, duplicate["isError"], "%#v", duplicate)
}

func TestCreatePersonToolRejectsCallerSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	backend := &creatingPeopleBackend{store: testutil.NewTestStore(t)}
	opts := personCreateToolOptions(backend)
	tool := toolsByName(t, rawListTools(t, opts, true))["create_person"]
	inputSchema, ok := tool["inputSchema"].(map[string]any)
	require.True(ok)
	assert.NotContains(inputSchema["properties"], "source")
	result := rawCallTool(t, opts, "create_person", map[string]any{"name": "Alex Example", "source": "user"})
	require.Equal(true, result["isError"])
	assert.Contains(toolErrorTextFromResult(t, result), "source")
	people, err := backend.store.ListPersonsContext(t.Context())
	require.NoError(err)
	assert.Empty(people)
}

func TestCreatePersonToolPreservesDaemonDuplicateGuidance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &creatingPeopleBackend{err: &daemonclient.APIError{Status: 409, Code: "person_contact_exists", Message: "Matches person 7; use msgvault person get 7"}}
	result := rawCallTool(t, personCreateToolOptions(backend), "create_person", map[string]any{"name": "Alex Example"})
	require.Equal(true, result["isError"])
	assert.Contains(toolErrorTextFromResult(t, result), "person get 7")
}
