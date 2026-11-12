package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonGetFirstAppearanceRealProjection(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	sender := f.EnsureParticipant("sender@example.test", "Sender Example", "example.test")
	senderPerson, _, err := f.Store.CreatePersonFromParticipant(sender)
	require.NoError(err)
	_, err = f.Store.UpdatePersonDisplayNameContext(t.Context(), senderPerson.ID, senderPerson.Revision, new("Saved Sender"))
	require.NoError(err)
	target := f.EnsureParticipant("target@example.test", "Target Example", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(target)
	require.NoError(err)
	at := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	id := f.NewMessage().WithSourceMessageID("intro-cli").WithSentAt(at).Create(t, f.Store)
	require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Sender Example"}))
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner, target}, []string{"Owner", "Target Example"}))
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	srv := api.NewServer(&config.Config{}, &storeAPIAdapter{store: f.Store}, nil, slog.New(slog.DiscardHandler))
	var oldDaemon, failState atomic.Bool
	router := srv.Router()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if oldDaemon.Load() && r.URL.Path == "/api/v1/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"5.0.0"}`))
			return
		}
		if failState.Load() && r.URL.Path == fmt.Sprintf("/api/v1/people/%d/contact-state", person.ID) {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: httpServer.URL, AllowInsecure: true}})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/people/%d/contact-state", httpServer.URL, person.ID), nil)
	require.NoError(err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(http.StatusOK, response.StatusCode)
	var state map[string]any
	require.NoError(json.NewDecoder(response.Body).Decode(&state))
	assert.Equal(fmt.Sprintf("message:%d", id), state["first_seen_ref"])
	assert.Equal("email_intro", state["first_contact_kind"])

	savedJSON := personJSON
	personJSON = true
	t.Cleanup(func() { personJSON = savedJSON })
	for _, args := range [][]string{{strconv.FormatInt(person.ID, 10)}, {"--vcard-uid", person.VCardUID}} {
		get := &cobra.Command{Use: personGetCmd.Use, Args: personGetCmd.Args, RunE: personGetCmd.RunE}
		get.Flags().String("vcard-uid", "", "")
		var got bytes.Buffer
		get.SetOut(&got)
		get.SetArgs(args)
		require.NoError(get.ExecuteContext(ctx))
		var profile map[string]any
		require.NoError(json.Unmarshal(got.Bytes(), &profile))
		contact, ok := profile["contact_state"].(map[string]any)
		require.True(ok)
		assert.Equal(state["first_seen_ref"], contact["first_seen_ref"])
	}
	for _, scenario := range []struct {
		name      string
		old, fail bool
	}{{"older daemon", true, false}, {"optional subresource unavailable", false, true}} {
		t.Run(scenario.name, func(t *testing.T) {
			assert := assertpkg.New(t)
			require := requirepkg.New(t)
			oldDaemon.Store(scenario.old)
			failState.Store(scenario.fail)
			for _, args := range [][]string{{strconv.FormatInt(person.ID, 10)}, {"--vcard-uid", person.VCardUID}} {
				get := &cobra.Command{Use: personGetCmd.Use, Args: personGetCmd.Args, RunE: personGetCmd.RunE}
				get.Flags().String("vcard-uid", "", "")
				var got bytes.Buffer
				get.SetOut(&got)
				get.SetArgs(args)
				err := get.ExecuteContext(ctx)
				if scenario.fail {
					require.ErrorContains(err, "read person contact state")
					continue
				}
				require.NoError(err)
				var profile map[string]any
				require.NoError(json.Unmarshal(got.Bytes(), &profile))
				assert.InDelta(float64(person.ID), profile["id"], 0)
				assert.NotContains(profile, "contact_state")
			}
		})
	}
}
