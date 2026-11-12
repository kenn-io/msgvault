package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonContactStateCommandRealProjection(t *testing.T) {
	f := storetest.New(t)
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	sender := f.EnsureParticipant("sender@example.test", "Sender Example", "example.test")
	senderPerson, _, err := f.Store.CreatePersonFromParticipant(sender)
	require.NoError(t, err)
	_, err = f.Store.UpdatePersonDisplayNameContext(t.Context(), senderPerson.ID, senderPerson.Revision, new("Saved Sender"))
	require.NoError(t, err)
	target := f.EnsureParticipant("target@example.test", "Target Example", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(target)
	require.NoError(t, err)
	at := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	id := f.NewMessage().WithSourceMessageID("intro-cli").WithSentAt(at).Create(t, f.Store)
	require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Sender Example"}))
	require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{owner, target}, []string{"Owner", "Target Example"}))
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
	require.NoError(t, err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(t, err)
	srv := api.NewServer(&config.Config{}, &storeAPIAdapter{store: f.Store}, nil, slog.New(slog.DiscardHandler))
	httpServer := httptest.NewServer(srv.Router())
	t.Cleanup(httpServer.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: httpServer.URL, AllowInsecure: true}})
	command := newPersonContactStateCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{strconv.FormatInt(person.ID, 10), "--json"})
	require.NoError(t, command.ExecuteContext(ctx))
	var state map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &state))
	assert.Equal(t, fmt.Sprintf("message:%d", id), state["first_seen_ref"])
	assert.Equal(t, "email_intro", state["first_contact_kind"])
	assert.NotContains(t, state, "first_contact_at")
	intro, ok := state["introducer"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(sender), intro["participant_id"])
	assert.Equal(t, "Saved Sender", intro["display_name"])
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/people/%d/contact-state", httpServer.URL, person.ID), nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var apiState map[string]any
	require.NoError(t, json.NewDecoder(response.Body).Decode(&apiState))
	assert.Equal(t, apiState, state)

	textCommand := newPersonContactStateCommand()
	var plain bytes.Buffer
	textCommand.SetOut(&plain)
	textCommand.SetArgs([]string{strconv.FormatInt(person.ID, 10)})
	require.NoError(t, textCommand.ExecuteContext(ctx))
	assert.Contains(t, plain.String(), fmt.Sprintf("Evidence: message:%d", id))
	assert.Contains(t, plain.String(), "Introducer: Saved Sender")

	savedJSON := personJSON
	personJSON = true
	t.Cleanup(func() { personJSON = savedJSON })
	for _, args := range [][]string{{strconv.FormatInt(person.ID, 10)}, {"--vcard-uid", person.VCardUID}} {
		get := &cobra.Command{Use: personGetCmd.Use, Args: personGetCmd.Args, RunE: personGetCmd.RunE}
		get.Flags().String("vcard-uid", "", "")
		var got bytes.Buffer
		get.SetOut(&got)
		get.SetArgs(args)
		require.NoError(t, get.ExecuteContext(ctx))
		var profile map[string]any
		require.NoError(t, json.Unmarshal(got.Bytes(), &profile))
		contact, ok := profile["contact_state"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, state["first_seen_ref"], contact["first_seen_ref"])
	}
}

func TestPersonContactStateRequiresCurrentDaemon(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/health", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"status":"ok","api_schema_version":"5.0.0"}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	command := newPersonContactStateCommand()
	command.SetArgs([]string{"1"})
	err := command.ExecuteContext(ctx)
	require.ErrorContains(t, err, "5.1.0 or newer")
}
