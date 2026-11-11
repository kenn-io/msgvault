package mcp

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNativeEventParamsRejectDuplicateBeforeForwarding(t *testing.T) {
	var p nativeEventParams
	require.NoError(t, p.UnmarshalJSON([]byte(`{"name":"msgvault.messages.changed","arguments":{"source_id":1,"source_id":2},"delivery":{"mode":"webhook","url":"https://example.com","secret":"x"}}`)))
	_, err := p.subscribe()
	assert.Error(t, err)
}
func TestNativeEventResultIncludesNullCursor(t *testing.T) {
	b, err := json.Marshal(&nativeEventResult{value: map[string]any{"cursor": nil, "truncated": false}, complete: true})
	require.NoError(t, err)
	assert.JSONEq(t, `{"cursor":null,"truncated":false,"resultType":"complete"}`, string(b))
}
