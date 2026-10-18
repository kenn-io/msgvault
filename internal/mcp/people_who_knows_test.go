package mcp

import (
	"context"
	"encoding/json/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/whoknows"
	"testing"
)

type testWhoKnowsBackend struct {
	peoplebrowser.Backend

	topic  string
	params whoknows.Params
}

func (b *testWhoKnowsBackend) FindWhoKnows(_ context.Context, topic string, p whoknows.Params) (*whoknows.Response, error) {
	b.topic = topic
	b.params = p
	return &whoknows.Response{Topic: topic, People: []whoknows.Ranked{}, UnsavedCandidates: []whoknows.UnsavedCandidate{}}, nil
}
func TestFindWhoKnowsTool(t *testing.T) {
	check := assert.New(t)
	backend := &testWhoKnowsBackend{}
	result := rawCallTool(t, ServeOptions{Engine: &querytest.MockEngine{}, PeopleBackend: backend, WhoKnowsBackend: backend}, ToolFindWhoKnows, map[string]any{"topic": "cloud networking", "limit": 4, "evidence": 0, "half_life_days": 0, "include_owner": true})
	check.NotEqual(true, result["isError"])
	body := toolStructuredContent(t, result)
	check.Equal("cloud networking", body["topic"])
	expected, err := json.Marshal(&whoknows.Response{Topic: "cloud networking", People: []whoknows.Ranked{}, UnsavedCandidates: []whoknows.UnsavedCandidate{}})
	require.NoError(t, err)
	actual, err := json.Marshal(body)
	require.NoError(t, err)
	check.JSONEq(string(expected), string(actual))
	check.Equal("cloud networking", backend.topic)
	check.Equal(4, backend.params.Limit)
	check.Equal(0, backend.params.Evidence)
	check.Equal(0, backend.params.HalfLifeDays)
	check.True(backend.params.IncludeOwner)
	def := findWhoKnowsDefinition(nil)
	check.Equal(toolAnnotations(true), def.annotations)
	require.NotNil(t, def.inputSchema)
	check.NotContains(def.inputSchema.Properties, "excerpts")
	check.Contains(def.inputSchema.Required, "topic")
	check.False(def.availability(capabilitiesFor(ServeOptions{})))
}
