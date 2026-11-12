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
	assert.Equal(t, "Sender Example", intro["display_name"])
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/people/%d/contact-state", httpServer.URL, person.ID), nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var apiState map[string]any
	require.NoError(t, json.NewDecoder(response.Body).Decode(&apiState))
	assert.Equal(t, apiState, state)
}
