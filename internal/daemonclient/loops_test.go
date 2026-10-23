package daemonclient_test

import (
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopSnoozeDeadlineRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	const alex = "alex@example.test"
	f.PersistEmail(storetest.EmailSpec{From: storetest.OwnerAddress, To: []string{alex}, Body: "Hi", SentAt: now.Add(-10 * 24 * time.Hour)})
	anchor := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{storetest.OwnerAddress}, Subject: "Contract", Body: "Can you send it?", SentAt: now.Add(-3 * 24 * time.Hour)})
	cfg := config.NewDefaultConfig()
	cfg.Server.APIKey = "loop-client-key"
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: f.Store, Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, Context: t.Context()})
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })

	until := now.Add(48 * time.Hour)
	snoozed, err := client.SnoozeLoop(t.Context(), anchor, until)
	require.NoError(err)
	require.NotNil(snoozed.SnoozedUntil)
	assert.WithinDuration(until, *snoozed.SnoozedUntil, time.Second)
}
