package cmd

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/threads"
)

const sourceTypeThreads = "threads"

type threadsCommandDeps struct {
	bind                 func(context.Context) threadsCommandDeps
	openStore            func() (*store.Store, func(), error)
	tokenManager         func() *threads.TokenManager
	oauthOptions         func(*threads.RequestBudget) threads.OAuthClientOptions
	oauthPayload         func() string
	newAPI               func(threads.TokenRecord, *threads.RequestBudget) (threads.API, error)
	databaseDSN          func() string
	rebuildCache         func(string) error
	now                  func() time.Time
	wait                 threads.WaitFunc
	limiter              func(string) *threads.RateLimiter
	registerSource       func(context.Context, *store.Store, threads.TokenRecord, time.Time) (*store.Source, error)
	postSourceMigrations func(*store.Store) error
}

var threadsSourceLimiters sync.Map

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
			return openWritableStoreAndInitForIngestInvocation(state)
		},
		tokenManager: func() *threads.TokenManager {
			if state == nil || state.cfg == nil {
				return threads.NewTokenManager("")
			}
			return threads.NewTokenManager(state.cfg.TokensDir())
		},
		oauthOptions: func(b *threads.RequestBudget) threads.OAuthClientOptions {
			return threads.OAuthClientOptions{HTTPClient: threads.NewBudgetedHTTPClient(&http.Client{Timeout: 30 * time.Second}, b)}
		},
		newAPI: func(record threads.TokenRecord, budget *threads.RequestBudget) (threads.API, error) {
			return threads.NewClient(threads.ClientOptions{HTTPClient: &http.Client{Timeout: 30 * time.Second}, AccessToken: record.AccessToken(), Budget: budget})
		},
		databaseDSN: func() string {
			if state == nil || state.cfg == nil {
				return ""
			}
			return state.cfg.DatabaseDSN()
		},
		rebuildCache: func(dsn string) error { return rebuildCacheAfterManualSync(dsn, state) },
		now:          time.Now,
		limiter: func(source string) *threads.RateLimiter {
			value, _ := threadsSourceLimiters.LoadOrStore(source, threads.NewRateLimiter(nil, nil))
			limiter, ok := value.(*threads.RateLimiter)
			if !ok {
				panic("threads source limiter has unexpected type")
			}
			return limiter
		},
		registerSource:       registerThreadsSource,
		postSourceMigrations: func(st *store.Store) error { return runPostSourceCreateMigrationsForInvocation(st, state) },
	}
}
