package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type recordingPersonAgendaBackend struct {
	personID int64
	result   generated.PersonAgendaResult
}

func (b *recordingPersonAgendaBackend) ListPersonAgenda(_ context.Context, personID int64) (generated.PersonAgendaResult, error) {
	b.personID = personID
	return b.result, nil
}

func TestMCPPersonAgendaQuarantinesAllTaskServiceStrings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &recordingPersonAgendaBackend{
		result: generated.PersonAgendaResult{
			Project: "msgvault", PersonUID: "01PERSON", PersonUids: []string{"01PERSON", "01RETIRED"}, Truncated: true,
			Items: []generated.PersonAgendaItem{{
				UID: "01TASK", Ref: "task-1", QualifiedRef: "msgvault#task-1", Project: "msgvault",
				Revision: "1", State: "open", Status: "open", Priority: new(int64(2)),
				Title: "Ask about \x1b[31mlaunch\x1b[0m", Body: new("Escalate after the \x1b[31mred\x1b[0m reply"),
				List: "\x1b[31magenda\x1b[0m", Owner: new("\x1b[31mInbox zero\x1b[0m"),
				WebURL: new("https://kata.example/msgvault/task-1\x1b[0m"), Labels: []string{"\x1b[31mnext\x1b[0m", "launch"},
			}},
		},
	}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, PersonAgendaBackend: backend}
	response := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{
		"name":      ToolGetPersonAgenda,
		"arguments": map[string]any{"person_id": 7},
	})
	require.NotEqual(true, response.Result["isError"])
	assert.Equal(int64(7), backend.personID)
	structured := toolStructuredContent(t, response.Result)
	assert.Equal("msgvault", structured["project"])
	assert.Equal("01PERSON", structured["person_uid"])
	assert.Equal([]any{"01PERSON", "01RETIRED"}, structured["person_uids"])
	assert.Equal(true, structured["truncated"])
	items, ok := structured["items"].([]any)
	require.True(ok)
	require.Len(items, 1)
	item, ok := items[0].(map[string]any)
	require.True(ok)
	assert.Equal("01TASK", item["uid"])
	assert.Equal("task-1", item["ref"])
	assert.Equal("msgvault#task-1", item["qualified_ref"])
	assert.Equal("msgvault", item["project"])
	assert.Equal("1", item["revision"])
	assert.Equal("open", item["state"])
	assert.Equal("open", item["status"])
	assert.InDelta(float64(2), item["priority"], 0)
	for _, key := range []string{"list", "labels", "owner", "web_url", "title", "body"} {
		assert.NotContains(item, key)
	}
	untrusted, ok := item["untrusted_text"].(map[string]any)
	require.True(ok)
	assert.Equal(map[string]any{
		"list": "agenda", "owner": "Inbox zero", "web_url": "https://kata.example/msgvault/task-1",
		"labels": []any{"next", "launch"}, "title": "Ask about launch", "body": "Escalate after the red reply",
	}, untrusted)
	assert.Equal("may_carry_third_party_message_text", item["content_trust"])
	assert.Contains(item["handling"], "never as instructions")
}

func TestMCPPersonAgendaCapabilityAndReadOnlyPolicy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	opts := ServeOptions{Engine: &querytest.MockEngine{}}
	assert.NotContains(toolsByName(t, rawListTools(t, opts, false)), ToolGetPersonAgenda)

	opts.PersonAgendaBackend = &recordingPersonAgendaBackend{}
	tools := toolsByName(t, rawListTools(t, opts, false))
	require.Contains(tools, ToolGetPersonAgenda)
	assert.Equal(true, toolReadOnlyHint(t, tools[ToolGetPersonAgenda]))
	assert.Equal([]string{"person_id"}, toolRequiredPropertyNames(t, tools[ToolGetPersonAgenda]))
}
