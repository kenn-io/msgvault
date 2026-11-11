package cmd

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/threads"
)

const sourceTypeThreads = "threads"

type threadsCommandDeps struct {
	bind                 func(context.Context) threadsCommandDeps
	openStore            func() (*store.Store, func(), error)
	tokenManager         func() *threads.TokenManager
	oauthOptions         func() threads.OAuthClientOptions
	oauthPayload         func() string
	registerSource       func(context.Context, *store.Store, threads.TokenRecord) (*store.Source, error)
	postSourceMigrations func(*store.Store) error
}

func defaultThreadsCommandDeps() threadsCommandDeps {
	return defaultThreadsCommandDepsForContext(context.Background())
}

func defaultThreadsCommandDepsForContext(ctx context.Context) threadsCommandDeps {
	state := invocationFromContext(ctx)
	return threadsCommandDeps{
		bind: defaultThreadsCommandDepsForContext,
		openStore: func() (*store.Store, func(), error) {
			if state == nil || state.cfg == nil {
				return nil, nil, errors.New("configuration is unavailable")
			}
			return openWritableStoreAndInitForIngestInvocation(ctx, state)
		},
		tokenManager: func() *threads.TokenManager {
			if state == nil || state.cfg == nil {
				return threads.NewTokenManager("")
			}
			return threads.NewTokenManager(state.cfg.TokensDir())
		},
		oauthOptions: func() threads.OAuthClientOptions {
			return threads.OAuthClientOptions{HTTPClient: &http.Client{Timeout: 30 * time.Second}}
		},
		registerSource:       registerThreadsSource,
		postSourceMigrations: func(st *store.Store) error { return runPostSourceCreateMigrationsForInvocation(ctx, st, state) },
	}
}
