package mcpevents

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
)

func TestOptionalResourceReferenceAdmissionBeforeChallenge(t *testing.T) {
	for _, mode := range []string{"valid", "source_instead_of_conversation", "forged", "stale", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			service, f, request := eventService(t)
			resource, err := f.Store.GetMCPResource(t.Context(), "conversation", f.ConvID)
			require.NoError(err)
			reference := resource.Reference
			switch mode {
			case "source_instead_of_conversation":
				source, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
				require.NoError(err)
				reference = source.Reference
			case "forged":
				reference = "mcp1." + strings.Repeat("A", 43)
			case "malformed":
				reference = "mcp1." + strings.Repeat("A", 42) + "B"
			case "stale":
				require.NoError(f.Store.UpdateSourceIdentifier(f.Source.ID, "replacement@example.net"))
				require.NoError(f.Store.UpdateSourceIdentifier(f.Source.ID, f.Source.Identifier))
			}
			request.Arguments["conversation_ref"] = reference
			var calls atomic.Int32
			service.webhook = tlsWebhook(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var value map[string]any
				if !assert.NoError(json.NewDecoder(r.Body).Decode(&value)) {
					return
				}
				assert.NoError(json.NewEncoder(w).Encode(map[string]any{"challenge": value["challenge"]}))
			})
			result, err := service.Subscribe(t.Context(), service.principal, request)
			if mode == "valid" {
				require.NoError(err)
				assert.NotEmpty(result.ID)
				assert.Equal(int32(1), calls.Load())
			} else {
				require.Error(err)
				assert.Zero(calls.Load(), "invalid reference must fail before receiver I/O")
				rows, err := f.Store.ListMCPSubscriptionsForStartup(t.Context())
				require.NoError(err)
				assert.Empty(rows)
			}
		})
	}
}
