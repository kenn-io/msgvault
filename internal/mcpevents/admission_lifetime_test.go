package mcpevents

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
)

func TestNewSubscriptionRejectsResourceRebindDuringChallenge(t *testing.T) {
	for _, awayBack := range []bool{false, true} {
		name := "replacement"
		if awayBack {
			name = "away_and_back"
		}
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			service, fixture, request := eventService(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			service.webhook = tlsWebhook(t, func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				var value map[string]any
				if !assert.NoError(json.NewDecoder(r.Body).Decode(&value)) {
					return
				}
				assert.NoError(json.NewEncoder(w).Encode(map[string]any{"challenge": value["challenge"]}))
			})
			t.Cleanup(unblock)
			done := make(chan error, 1)
			go func() { _, err := service.Subscribe(t.Context(), service.principal, request); done <- err }()
			select {
			case <-entered:
			case <-time.After(30 * time.Second):
				require.FailNow("challenge did not start")
			}
			// Native source mutation must complete while the network challenge is held.
			// No Events/identity SQL transaction may span the callback.
			changed := make(chan error, 1)
			go func() { changed <- fixture.Store.UpdateSourceIdentifier(fixture.Source.ID, "replacement@example.net") }()
			select {
			case err := <-changed:
				require.NoError(err)
			case <-time.After(30 * time.Second):
				require.FailNow("source mutation blocked behind callback")
			}
			if awayBack {
				require.NoError(fixture.Store.UpdateSourceIdentifier(fixture.Source.ID, fixture.Source.Identifier))
			}
			unblock()
			select {
			case err := <-done:
				require.Error(err, "resource rebound during challenge must not be admitted")
			case <-time.After(30 * time.Second):
				require.FailNow("challenge did not settle")
			}
			rows, err := fixture.Store.ListMCPSubscriptionsForStartup(t.Context())
			require.NoError(err)
			assert.Empty(rows, "failed admission must not leave durable authority")
		})
	}
}
