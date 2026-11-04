package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

type relationshipPeopleBackend struct {
	peoplebrowser.Backend

	page    *peoplebrowser.RelationshipListPage
	err     error
	request peoplebrowser.RelationshipListRequest
}

func (b *relationshipPeopleBackend) ListRelationships(
	_ context.Context, request peoplebrowser.RelationshipListRequest,
) (*peoplebrowser.RelationshipListPage, error) {
	b.request = request
	return b.page, b.err
}

func relationshipToolOptions(backend peoplebrowser.Backend) ServeOptions {
	return ServeOptions{Engine: &querytest.MockEngine{}, PeopleBackend: backend}
}

func TestMCPListRelationshipsForwardsCadenceAndReturnsJoinedRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	last := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	backend := &relationshipPeopleBackend{page: &peoplebrowser.RelationshipListPage{
		Rows: []query.RelationshipRow{{
			CanonicalID: 7, DisplayLabel: "Alice Example", MemberIDs: []int64{7}, Score: 4,
			Signals: query.RelationshipSignals{SentCount: 2, LastInteractionAt: last}, LastAt: last,
			PersonID: new(int64(11)),
		}},
		TotalCount: 1, CacheRevision: "cache-rel", IdentityRevision: 3, NextCursor: "next",
	}}
	result := rawCallTool(t, relationshipToolOptions(backend), ToolListRelationships, map[string]any{
		"cursor": "cursor", "limit": float64(12), "show_all": true,
		"cadence": "due", "due_within_days": float64(7),
	})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	assert.Equal(peoplebrowser.RelationshipListRequest{
		Cursor: "cursor", Limit: 12, ShowAll: true, Cadence: "due", DueWithinDays: new(7),
	}, backend.request)
	structured := toolStructuredContent(t, result)
	assert.Equal("next", structured["next_cursor"])
	rows, ok := structured["rows"].([]any)
	require.True(ok)
	require.Len(rows, 1)
	row, ok := rows[0].(map[string]any)
	require.True(ok)
	assert.InDelta(7, row["canonical_id"], 0)
	assert.Equal("Alice Example", row["display_label"])
	assert.InDelta(11, row["person_id"], 0)
}

func TestMCPListRelationshipsValidatesCadenceArguments(t *testing.T) {
	for _, test := range []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "invalid cadence", args: map[string]any{"cadence": "later"}, want: "enum"},
		{name: "horizon without due", args: map[string]any{"due_within_days": float64(7)}, want: "due_within_days requires cadence=due"},
		{name: "zero limit", args: map[string]any{"limit": float64(0)}, want: "limit must be between 1 and 500"},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &relationshipPeopleBackend{page: &peoplebrowser.RelationshipListPage{}}
			result := rawCallTool(t, relationshipToolOptions(backend), ToolListRelationships, test.args)
			assert.Equal(t, true, result["isError"])
			assert.Contains(t, toolErrorTextFromResult(t, result), test.want)
			assert.Zero(t, backend.request)
		})
	}
}

func TestMCPListRelationshipsIsOptionalPeopleCapability(t *testing.T) {
	without := toolsByName(t, rawListTools(t, ServeOptions{Engine: &querytest.MockEngine{}}, false))
	assert.NotContains(t, without, ToolListRelationships)

	backend := &relationshipPeopleBackend{page: &peoplebrowser.RelationshipListPage{}}
	with := toolsByName(t, rawListTools(t, relationshipToolOptions(backend), false))
	assert.Contains(t, with, ToolListRelationships)
	assert.Equal(t, true, toolReadOnlyHint(t, with[ToolListRelationships]))
}
