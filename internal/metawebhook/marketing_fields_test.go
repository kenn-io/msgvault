package metawebhook

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventEnvelopeChangesOrMessaging(t *testing.T) {
	for _, field := range []string{"marketing_message_echoes", "marketing_message_deliveries", "marketing_message_reads", "marketing_message_clicks", "marketing_message_delivery_failed", "messaging_optins"} {
		t.Run(field, func(t *testing.T) {
			member := "{\"" + field + "\":{\"mid\":\"mid-1\"}}"
			if field == "messaging_optins" {
				member = `{"optin":{"type":"notification_messages"}}`
			}
			for _, part := range []string{fmt.Sprintf(`"changes":[{"field":%q,"value":{"mid":"mid-1"}}]`, field), `"messaging":[` + member + `]`} {
				requirements := require.New(t)
				items, err := Decode("9", []byte(`{"object":"page","entry":[{"id":"100",`+part+`}]}`))
				requirements.NoError(err)
				requirements.Len(items, 1)
				assert.Equal(t, field, items[0].Field)
				reg := NewRegistry()
				requirements.NotPanics(func() {
					reg.Register("page", field, func(context.Context, string, []Item) []ItemOutcome { return nil })
				})
			}
		})
	}
}
