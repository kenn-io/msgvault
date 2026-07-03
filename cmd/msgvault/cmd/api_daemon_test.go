package cmd

import (
	"log/slog"
	"net/http/httptest"
	"testing"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func startStoreAPIDaemon(
	t *testing.T,
	dataDir string,
	st *store.Store,
	engine query.Engine,
) {
	t.Helper()

	apiCfg := &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config:        apiCfg,
		Store:         st,
		Engine:        engine,
		Logger:        slog.New(slog.DiscardHandler),
		DaemonVersion: Version,
	})
	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)
	writeStatsHTTPDaemonRuntime(t, dataDir, httpSrv)
}

func startStoreQueryAPIDaemon(t *testing.T, dataDir string, st *store.Store) {
	t.Helper()

	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })
	startStoreAPIDaemon(t, dataDir, st, engine)
}
