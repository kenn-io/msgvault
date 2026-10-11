package client

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// Required context text means present, not nonempty. Exercise the compiled
// generated validator and JSON round trip rather than checking emitted source.
func TestGeneratedInboxContextAcceptsPresentEmptyText(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	target := `"target":{"source_id":1,"source_type":"gmail","source_identifier":"reader@example.test","account_id":"reader@example.test","scope":"message","item_id":1,"provider_id":"context-empty"}`
	for _, body := range []string{
		`{"message_id":1,` + target + `,"text":"","truncated":false,"unavailable":false}`,
		`{"message_id":1,` + target + `,"text":"","truncated":false,"unavailable":true}`,
		`{"message_id":1,` + target + `,"text":"Synthetic text","truncated":true,"unavailable":false}`,
	} {
		var value generated.InboxContext
		requirements.NoError(json.Unmarshal([]byte(body), &value))
		requirements.NoError(value.Validate(), body)
		encoded, err := json.Marshal(value)
		requirements.NoError(err)
		assertions.JSONEq(body, string(encoded))
	}
	for _, text := range []string{"", `,"text":null`} {
		var value generated.InboxContext
		requirements.NoError(json.Unmarshal([]byte(`{"message_id":1,`+target+text+`,"truncated":false,"unavailable":false}`), &value))
		requirements.Error(value.Validate(), "missing or null text is not a known empty body")
	}
	var foreign generated.InboxContext
	requirements.NoError(json.Unmarshal([]byte(`{"message_id":1,`+strings.Replace(target, `"provider_id":"context-empty"`, `"provider_id":""`, 1)+`,"text":"Synthetic text","truncated":false,"unavailable":false}`), &foreign))
	assertions.Error(foreign.Validate(), "target validation remains required")
}

func TestGeneratedInboxContextRequestByteBounds(t *testing.T) {
	for _, bound := range []int64{-1, 0, 1, 65536, 65537} {
		request := generated.InboxContextRequest{Target: generated.InboxTarget{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.test", AccountID: "reader@example.test", Scope: "message", ItemID: 1, ProviderID: "context-bounds"}, MaxBytes: &bound}
		if bound < 0 || bound > 65536 {
			assert.Error(t, request.Validate(), "byte bound %d", bound)
		} else {
			assert.NoError(t, request.Validate(), "byte bound %d", bound)
		}
	}
}
