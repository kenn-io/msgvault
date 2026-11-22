package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/store"
)

func TestMCPSetPersonContactList(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, existing := range []bool{false, true} {
		group := peoplebrowser.AttributeGroup{Definition: store.AttributeDefinition{UniversalID: store.AttributeUniversalIDContactFrequency, Slug: "renamed_frequency"}}
		args := map[string]any{"person_id": 7, "list": "A"}
		if existing {
			group.Current = []store.PersonAttributeValue{{ID: 71}}
			args["expected_value_id"] = 71
		}
		backend := &recordingPeopleBackend{attributes: &peoplebrowser.Attributes{Groups: []peoplebrowser.AttributeGroup{group}}, setWrite: &store.PersonAttributeWrite{Value: &store.PersonAttributeValue{ID: 72}}}
		result := confirmedCallTool(t, peopleToolOptions(backend), "set_person_contact_list", args, true)
		require.NotEqual(true, result["isError"], "result: %#v", result)
		assert.Equal("renamed_frequency", backend.setRequest.Slug)
		require.NotNil(backend.setRequest.Value.Integer)
		assert.Equal(int64(21), *backend.setRequest.Value.Integer)
		assert.Equal(store.ProvenanceEnrichment, backend.setRequest.Source)
		assert.Equal("mcp", backend.setRequest.Actor)
		require.NotNil(backend.setRequest.ExpectedValueID)
		if !existing {
			assert.Zero(*backend.setRequest.ExpectedValueID)
		}
		if existing {
			require.NotNil(backend.setRequest.ExpectedValueID)
			assert.Equal(int64(71), *backend.setRequest.ExpectedValueID)
		}
		backend.setRequest = peoplebrowser.SetAttributeRequest{}
		denied := confirmedCallTool(t, peopleToolOptions(backend), "set_person_contact_list", args, false)
		assert.Equal(true, denied["isError"])
		assert.Zero(backend.setRequest)
	}
}
func TestMCPContactListWriteGate(t *testing.T) {
	assert := assert.New(t)
	backend := &recordingPeopleBackend{}
	assert.NotContains(toolsByName(t, rawListTools(t, peopleToolOptions(backend), false)), "set_person_contact_list")
	result := rawCallTool(t, peopleToolOptions(backend), "set_person_contact_list", map[string]any{"person_id": 7, "list": "Z"})
	assert.Equal(true, result["isError"])
	assert.Zero(backend.attributesPersonID)
}
