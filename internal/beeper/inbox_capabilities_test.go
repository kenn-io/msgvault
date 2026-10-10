package beeper

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxBeeperSourceCapabilitiesRequireLiveAccountEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantError  error
	}{
		{"account present", `[{"accountID":"account-a","network":"Synthetic"}]`, 200, nil},
		{"foreign account", `[{"accountID":"account-b"}]`, 200, inboxcontrol.ErrUnavailable},
		{"duplicate account", `[{"accountID":"account-a"},{"accountID":"account-a"}]`, 200, inboxcontrol.ErrUnavailable},
		{"malformed", `{}`, 200, inboxcontrol.ErrUnavailable},
		{"offline", `synthetic secret`, 503, inboxcontrol.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			gets, posts := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					gets++
				} else {
					posts++
				}
				assert.Equal(t, "/v1/accounts", r.URL.Path)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "beeper", SourceIdentifier: "account-a", AccountID: "account-a"}
			provider := NewInboxProvider(NewClient(server.URL, testToken, 10000), source)
			reader, ok := any(provider).(inboxcontrol.CapabilityProvider)
			requirements.True(ok, "native provider must expose source capabilities")
			request := inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source}
			got, err := reader.Capabilities(t.Context(), request)
			if tc.wantError != nil {
				require.ErrorIs(t, err, tc.wantError)
				assertions.Nil(got)
			} else {
				requirements.NoError(err)
				requirements.NotNil(got)
				assertions.Equal(source, got.Source)
				assertions.Equal("chat-archive", got.LocationModel)
				assertions.False(got.ConditionalWrite)
				assertions.False(got.ObservedAt.IsZero())
				capabilities := map[inboxcontrol.Operation]inboxcontrol.CapabilityStatus{}
				for _, capability := range got.Operations {
					capabilities[capability.Operation] = capability.Status
				}
				assertions.Equal(inboxcontrol.CapabilitySupported, capabilities[inboxcontrol.OpGetState])
				for _, operation := range []inboxcontrol.Operation{inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread} {
					assertions.Equal(inboxcontrol.CapabilityUnavailable, capabilities[operation], "native support requires an exact chat observation")
				}
				for _, operation := range []inboxcontrol.Operation{inboxcontrol.OpTags, inboxcontrol.OpMove, inboxcontrol.OpListFolders, inboxcontrol.OpCreateFolder} {
					assertions.Equal(inboxcontrol.CapabilityUnsupported, capabilities[operation])
				}
			}
			assertions.Equal(1, gets)
			assertions.Equal(0, posts)
			foreign := source
			foreign.AccountID = "account-b"
			_, err = reader.Capabilities(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &foreign})
			require.ErrorIs(t, err, inboxcontrol.ErrDenied)
			assertions.Equal(1, gets, "injected binding must fail before provider access")
		})
	}
}
