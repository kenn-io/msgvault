package mcp

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// The tier tools read the daemon's HTTP routes, so this drives a real store
// through the API server and daemon client the way msgvault mcp does.
func TestMCPPersonTierToolsReadTheDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	friendParticipant := f.EnsureParticipant("friend@example.com", "Friend", "example.com")
	friend, _, err := f.Store.CreatePersonFromParticipant(friendParticipant)
	require.NoError(err)
	quietParticipant := f.EnsureParticipant("quiet@example.com", "Quiet", "example.com")
	quiet, _, err := f.Store.CreatePersonFromParticipant(quietParticipant)
	require.NoError(err)
	// Eight days of alternating mail with the friend reaches tier 1; one
	// inbound message from the quiet person reaches no tier.
	now := time.Now().UTC()
	for i := range 8 {
		from, to := friendParticipant, owner
		if i%2 == 0 {
			from, to = owner, friendParticipant
		}
		seedTierToolMessage(t, f, fmt.Sprintf("friend-%d", i), now.AddDate(0, 0, -i), from, to)
	}
	seedTierToolMessage(t, f, "quiet-0", now, quietParticipant, owner)
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 500, MaxDirectCounterparts: 25})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)

	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Activity.Timezone, cfg.Activity.MaxDirectCounterparts = "UTC", 25
	daemon := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: cfg, Store: f.Store, Logger: slog.New(slog.DiscardHandler),
	}).Router())
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AllowInsecure: true, HTTPClient: daemon.Client()})
	require.NoError(err)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, PersonTierBackend: client}

	tools := toolsByName(t, rawListTools(t, opts, false))
	require.Contains(tools, ToolPreviewPersonTiers)
	assert.Equal(true, toolReadOnlyHint(t, tools[ToolPreviewPersonTiers]))
	assert.Equal([]string{"person_id"}, toolRequiredPropertyNames(t, tools[ToolExplainPersonTier]))

	preview := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{
		"name": ToolPreviewPersonTiers, "arguments": map[string]any{},
	})
	require.NotEqual(true, preview.Result["isError"])
	structured := toolStructuredContent(t, preview.Result)
	assert.Equal(map[string]any{"ready": true, "reasons": []any{}}, structured["readiness"])
	people, ok := structured["people"].([]any)
	require.True(ok)
	require.Len(people, 1)
	row, ok := people[0].(map[string]any)
	require.True(ok)
	assert.InDelta(float64(friend.ID), row["person_id"], 0)
	assert.InDelta(float64(1), row["tier"], 0)
	assert.Equal("tier1_intensity", row["rule"])
	assert.Equal(false, row["tracked"])

	explain := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{
		"name": ToolExplainPersonTier, "arguments": map[string]any{"person_id": quiet.ID},
	})
	require.NotEqual(true, explain.Result["isError"])
	explained := toolStructuredContent(t, explain.Result)
	assert.InDelta(float64(3), explained["tier"], 0)
	assert.Equal("none", explained["rule"])
	assert.Nil(explained["exclusion"])
	signals, ok := explained["signals"].(map[string]any)
	require.True(ok)
	assert.InDelta(float64(1), signals["inbound_short"], 0)

	missing := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{
		"name": ToolExplainPersonTier, "arguments": map[string]any{"person_id": quiet.ID + 1000},
	})
	assert.Equal(true, missing.Result["isError"])
	assert.Contains(fmt.Sprint(missing.Result["content"]), "person_profile_not_found")
}

func seedTierToolMessage(t *testing.T, f *storetest.Fixture, id string, sentAt time.Time, from, to int64) {
	t.Helper()
	message := f.NewMessage().WithSourceMessageID("tier-tool-" + id).WithSentAt(sentAt).Build()
	message.SenderID = sql.NullInt64{Int64: from, Valid: true}
	messageID, err := f.Store.UpsertMessage(message)
	require.NoError(t, err)
	require.NoError(t, f.Store.ReplaceMessageRecipients(messageID, "from", []int64{from}, nil))
	require.NoError(t, f.Store.ReplaceMessageRecipients(messageID, "to", []int64{to}, nil))
}
