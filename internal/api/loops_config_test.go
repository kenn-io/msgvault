package api

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopFreshnessUsesCurrentConfiguredAccounts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := storetest.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Loops.Enabled = true
	service, err := loops.NewService(fixture.Store, cfg, nil)
	requirements.NoError(err)
	server := NewServerWithOptions(ServerOptions{
		Config:      cfg,
		Store:       fixture.Store,
		LoopService: service,
		Logger:      slog.New(slog.DiscardHandler),
	})

	account := config.AccountSchedule{Email: "new@example.test", Enabled: true}
	server.cfgMu.Lock()
	server.cfg.Accounts = append(server.cfg.Accounts, account)
	server.cfgMu.Unlock()

	report, err := service.Freshness(t.Context(), time.Now().UTC(), nil)
	requirements.NoError(err)
	var found bool
	for _, source := range report.Sources {
		if source.Identifier == account.Email {
			found = true
			assertions.Nil(source.SourceID)
			assertions.True(source.Stale)
		}
	}
	assertions.True(found, "freshness must include configured accounts added after service construction")
}
