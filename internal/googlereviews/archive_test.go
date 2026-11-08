package googlereviews

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type archiveTestStore struct {
	mu        sync.Mutex
	state     BackfillState
	begins    int
	reviews   map[string]Review
	done      chan struct{}
	signaled  bool
	search    ArchiveSearchResponse
	searchErr error
}

func (a *archiveTestStore) BeginGoogleReviewsBackfill(_ context.Context, target, location string) (BackfillState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.begins++
	if a.state.Status != "running" {
		a.state = BackfillState{Target: target, Location: location, Status: "running", RunID: 1, StartedAt: new(time.Now())}
	}
	return a.state, nil
}

func (a *archiveTestStore) ApplyGoogleReviewsPage(_ context.Context, target, location, expected string, page Page) (BackfillState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.NextPageToken != expected || a.state.Status != "running" {
		return BackfillState{}, ErrArchiveBusy
	}
	if a.reviews == nil {
		a.reviews = make(map[string]Review)
	}
	for _, review := range page.Reviews {
		a.reviews[review.ReviewID] = review
	}
	a.state.PagesFetched++
	a.state.ReviewsFetched += int64(len(page.Reviews))
	a.state.TotalReviewCount = page.TotalReviewCount
	a.state.ArchivedReviewCount = int64(len(a.reviews))
	a.state.NextPageToken = page.NextPageToken
	if page.NextPageToken == "" {
		a.state.Status = "api_exhausted"
		completed := time.Now()
		a.state.CompletedAt = &completed
		if a.done != nil && !a.signaled {
			close(a.done)
			a.signaled = true
		}
	}
	return a.state, nil
}

func (a *archiveTestStore) FailGoogleReviewsBackfill(_ context.Context, target, location, code string) (BackfillState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Target = target
	a.state.Location = location
	a.state.Status = "partial"
	a.state.LastErrorCode = code
	if a.done != nil && !a.signaled {
		close(a.done)
		a.signaled = true
	}
	return a.state, nil
}

func (a *archiveTestStore) GoogleReviewsBackfillState(_ context.Context, target, _ string) (BackfillState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.state
	if state.Target == "" {
		state = BackfillState{Target: target, Status: "not_started"}
	}
	return state, nil
}

func (a *archiveTestStore) SearchGoogleReviews(_ context.Context, query, target string, offset, limit int) (ArchiveSearchResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.searchErr != nil {
		return ArchiveSearchResponse{}, a.searchErr
	}
	response := a.search
	response.Query, response.Target = query, target
	response.Page, response.PageSize = offset/limit+1, limit
	return response, nil
}

type archiveTestProvider struct {
	pages       map[string]Page
	calls       int
	pageSizes   []int
	puts        int
	err         error
	listEntered chan struct{}
	listRelease chan struct{}
	onList      func()
}

func (p *archiveTestProvider) List(ctx context.Context, _, token string, pageSize int) (Page, error) {
	p.calls++
	p.pageSizes = append(p.pageSizes, pageSize)
	if p.listEntered != nil {
		select {
		case p.listEntered <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return Page{}, ctx.Err()
		case <-p.listRelease:
		}
	}
	if p.err != nil {
		return Page{}, p.err
	}
	if p.onList != nil {
		p.onList()
	}
	page, ok := p.pages[token]
	if !ok {
		return Page{}, errors.New("unexpected synthetic page token")
	}
	return page, nil
}

func (p *archiveTestProvider) Get(context.Context, string) (Review, error) {
	return Review{}, ErrNotFound
}

func (p *archiveTestProvider) UpdateReply(context.Context, string, string) (Reply, error) {
	p.puts++
	return Reply{}, nil
}

func TestBackfillResumesPagesAndNeverWritesProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const location = "accounts/a/locations/l"
	archive := &archiveTestStore{done: make(chan struct{})}
	provider := &archiveTestProvider{pages: map[string]Page{
		"":         {Reviews: []Review{{Name: location + "/reviews/r1", ReviewID: "r1", Comment: "synthetic first"}}, NextPageToken: "page-two", TotalReviewCount: 2},
		"page-two": {Reviews: []Review{{Name: location + "/reviews/r2", ReviewID: "r2", Comment: "synthetic second"}}, TotalReviewCount: 2},
	}}
	target := Target{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Location: location, Enabled: true, OwnProject: true}
	service := NewService(Options{
		Archive:  archive,
		Targets:  func(context.Context) ([]Target, error) { return []Target{target}, nil },
		Provider: func(context.Context, Target) (Provider, string, error) { return provider, "synthetic-binding", nil },
	})
	t.Cleanup(service.Close)

	_, err := service.Execute(t.Context(), "", Request{Action: "backfill", Target: "shop"})
	require.ErrorIs(err, ErrDenied)
	result, err := service.Execute(t.Context(), "owner", Request{Action: "backfill", Target: "shop"})
	require.NoError(err)
	require.NotNil(result.ArchiveSync)
	assert.Equal("running", result.ArchiveSync.Status)

	select {
	case <-archive.done:
	case <-time.After(30 * time.Second):
		require.FailNow("backfill did not reach an observable completed state")
	}
	status, err := service.Execute(t.Context(), "owner", Request{Action: "archive_status", Target: "shop"})
	require.NoError(err)
	require.NotNil(status.ArchiveSync)
	assert.Equal("api_exhausted", status.ArchiveSync.Status)
	assert.Equal(int64(2), status.ArchiveSync.PagesFetched)
	assert.Equal(int64(2), status.ArchiveSync.ReviewsFetched)
	assert.Equal(backfillCoverageNote, status.ArchiveSync.CoverageNote)
	assert.Equal([]int{50, 50}, provider.pageSizes)
	assert.Zero(provider.puts)
}

func TestBackfillPersistsOnlyClassifiedProviderFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const location = "accounts/a/locations/l"
	archive := &archiveTestStore{done: make(chan struct{})}
	provider := &archiveTestProvider{err: ErrQuota}
	target := Target{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Location: location, Enabled: true, OwnProject: true}
	service := NewService(Options{
		Archive:  archive,
		Targets:  func(context.Context) ([]Target, error) { return []Target{target}, nil },
		Provider: func(context.Context, Target) (Provider, string, error) { return provider, "synthetic-binding", nil },
	})
	t.Cleanup(service.Close)
	_, err := service.Execute(t.Context(), "owner", Request{Action: "backfill", Target: "shop"})
	require.NoError(err)

	select {
	case <-archive.done:
	case <-time.After(30 * time.Second):
		require.FailNow("backfill failure was not persisted")
	}
	status, statusErr := service.Execute(t.Context(), "owner", Request{Action: "archive_status", Target: "shop"})
	require.NoError(statusErr)
	require.NotNil(status.ArchiveSync)
	assert.Equal("partial", status.ArchiveSync.Status)
	assert.Equal("quota", status.ArchiveSync.LastErrorCode)
	assert.Zero(provider.puts)
}

func TestConcurrentBackfillRequestsShareOneWorker(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const location = "accounts/a/locations/l"
	archive := &archiveTestStore{done: make(chan struct{})}
	provider := &archiveTestProvider{
		pages:       map[string]Page{"": {Reviews: []Review{}, TotalReviewCount: 0}},
		listEntered: make(chan struct{}, 1),
		listRelease: make(chan struct{}),
	}
	target := Target{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Location: location, Enabled: true, OwnProject: true}
	service := NewService(Options{
		Archive:  archive,
		Targets:  func(context.Context) ([]Target, error) { return []Target{target}, nil },
		Provider: func(context.Context, Target) (Provider, string, error) { return provider, "synthetic-binding", nil },
	})
	t.Cleanup(service.Close)

	_, err := service.Execute(t.Context(), "owner", Request{Action: "backfill", Target: "shop"})
	require.NoError(err)
	select {
	case <-provider.listEntered:
	case <-time.After(30 * time.Second):
		require.FailNow("backfill worker did not start its provider read")
	}

	result, err := service.Execute(t.Context(), "owner", Request{Action: "backfill", Target: "shop"})
	require.NoError(err)
	require.NotNil(result.ArchiveSync)
	assert.Equal("running", result.ArchiveSync.Status)
	assert.Equal(1, provider.calls)
	archive.mu.Lock()
	assert.Equal(1, archive.begins)
	archive.mu.Unlock()

	close(provider.listRelease)
	select {
	case <-archive.done:
	case <-time.After(30 * time.Second):
		require.FailNow("backfill worker did not finish its observable archive update")
	}
	assert.Zero(provider.puts)
}

var _ Archive = (*archiveTestStore)(nil)
var _ Provider = (*archiveTestProvider)(nil)

func TestInvalidateSettlesBackfillAndRejectsStaleResume(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const location = "accounts/a/locations/l"
	archive := &archiveTestStore{done: make(chan struct{})}
	provider := &archiveTestProvider{pages: map[string]Page{"": {}}, listEntered: make(chan struct{}, 1), listRelease: make(chan struct{})}
	shop := Target{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Location: location, Enabled: true, OwnProject: true}
	alias := shop
	alias.Name = "alias"
	service := NewService(Options{
		Archive:  archive,
		Targets:  func(context.Context) ([]Target, error) { return []Target{shop, alias}, nil },
		Provider: func(context.Context, Target) (Provider, string, error) { return provider, "binding", nil },
	})
	t.Cleanup(service.Close)
	_, err := service.Execute(t.Context(), "owner", Request{Action: "backfill", Target: "alias"})
	require.NoError(err)
	select {
	case <-provider.listEntered:
	case <-time.After(30 * time.Second):
		require.FailNow("provider read did not start")
	}
	first, err := service.Execute(t.Context(), "owner", Request{Action: "invalidate_target", Target: "shop"})
	require.NoError(err)
	require.NotEmpty(first.CredentialChangeToken)
	archive.mu.Lock()
	assert.Equal("partial", archive.state.Status)
	assert.Empty(archive.reviews)
	archive.mu.Unlock()
	_, err = service.Execute(t.Context(), "owner", Request{Action: "backfill", Target: "alias"})
	require.ErrorIs(err, ErrCredentials)
	second, err := service.Execute(t.Context(), "owner", Request{Action: "invalidate_target", Target: "shop"})
	require.NoError(err)
	_, err = service.Execute(t.Context(), "owner", Request{Action: "resume_target", Target: "shop", ConfirmationToken: first.CredentialChangeToken})
	require.ErrorIs(err, ErrConflict)
	_, err = service.Execute(t.Context(), "owner", Request{Action: "resume_target", Target: "shop", ConfirmationToken: second.CredentialChangeToken})
	require.NoError(err)
	close(provider.listRelease)
	_, err = service.Execute(t.Context(), "owner", Request{Action: "backfill", Target: "alias"})
	require.NoError(err)
}

func TestBackfillStopsWhenTargetDisabledBetweenPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const location = "accounts/a/locations/l"
	archive := &archiveTestStore{done: make(chan struct{})}
	var mu sync.Mutex
	target := Target{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Location: location, Enabled: true, OwnProject: true}
	provider := &archiveTestProvider{pages: map[string]Page{"": {NextPageToken: "next"}}}
	provider.onList = func() { mu.Lock(); target.Enabled = false; mu.Unlock() }
	service := NewService(Options{
		Archive:  archive,
		Targets:  func(context.Context) ([]Target, error) { mu.Lock(); defer mu.Unlock(); return []Target{target}, nil },
		Provider: func(context.Context, Target) (Provider, string, error) { return provider, "binding", nil },
	})
	t.Cleanup(service.Close)
	_, err := service.Execute(t.Context(), "owner", Request{Action: "backfill", Target: "shop"})
	require.NoError(err)
	select {
	case <-archive.done:
	case <-time.After(30 * time.Second):
		require.FailNow("configuration change was not recorded")
	}
	archive.mu.Lock()
	assert.Equal("configuration_changed", archive.state.LastErrorCode)
	assert.Equal(int64(1), archive.state.PagesFetched)
	archive.mu.Unlock()
	assert.Equal(1, provider.calls)
}

func TestBackfillChecksProviderAgainForEachPage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const location = "accounts/a/locations/l"
	archive := &archiveTestStore{done: make(chan struct{})}
	first := &archiveTestProvider{pages: map[string]Page{"": {NextPageToken: "second"}}}
	refreshed := &archiveTestProvider{pages: map[string]Page{"second": {}}}
	target := Target{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Location: location, Enabled: true, OwnProject: true}
	var calls int
	service := NewService(Options{
		Archive: archive,
		Targets: func(context.Context) ([]Target, error) { return []Target{target}, nil },
		Provider: func(context.Context, Target) (Provider, string, error) {
			calls++
			if calls == 1 {
				return first, "first-token", nil
			}
			return refreshed, "refreshed-token", nil
		},
	})
	t.Cleanup(service.Close)
	_, err := service.Execute(t.Context(), "owner", Request{Action: "backfill", Target: "shop"})
	require.NoError(err)
	select {
	case <-archive.done:
	case <-time.After(30 * time.Second):
		require.FailNow("backfill did not finish")
	}
	assert.Equal(2, calls)
	assert.Equal(1, first.calls)
	assert.Equal(1, refreshed.calls)
	archive.mu.Lock()
	assert.Equal("api_exhausted", archive.state.Status)
	archive.mu.Unlock()
}

func TestBackfillHoldsArchiveGateOnlyForWrites(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const location = "accounts/a/locations/l"
	archive := &archiveTestStore{done: make(chan struct{})}
	gate := make(chan struct{}, 1)
	gate <- struct{}{} // Simulate a backup freeze holding the daemon gate.
	defer func() {
		select {
		case <-gate:
		default:
		}
	}()
	attempt := make(chan struct{}, 1)
	var acquisitions atomic.Int32
	var providerBlocked atomic.Bool
	provider := &archiveTestProvider{pages: map[string]Page{"": {}}}
	provider.onList = func() {
		select {
		case gate <- struct{}{}:
			<-gate
		default:
			providerBlocked.Store(true)
		}
	}
	target := Target{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Location: location, Enabled: true, OwnProject: true}
	service := NewService(Options{
		Archive:  archive,
		Targets:  func(context.Context) ([]Target, error) { return []Target{target}, nil },
		Provider: func(context.Context, Target) (Provider, string, error) { return provider, "binding", nil },
		BeginArchiveWrite: func(ctx context.Context) (func(), bool) {
			select {
			case attempt <- struct{}{}:
			default:
			}
			select {
			case gate <- struct{}{}:
				acquisitions.Add(1)
				return func() { <-gate }, true
			case <-ctx.Done():
				return nil, false
			}
		},
	})
	t.Cleanup(service.Close)
	started := make(chan error, 1)
	go func() {
		_, err := service.Execute(t.Context(), "owner", Request{Action: "backfill", Target: "shop"})
		started <- err
	}()
	select {
	case <-attempt:
	case <-time.After(30 * time.Second):
		require.FailNow("backfill did not request the archive gate")
	}
	archive.mu.Lock()
	assert.Zero(archive.begins)
	archive.mu.Unlock()
	<-gate
	select {
	case err := <-started:
		require.NoError(err)
	case <-time.After(30 * time.Second):
		require.FailNow("backfill did not start after gate release")
	}
	select {
	case <-archive.done:
	case <-time.After(30 * time.Second):
		require.FailNow("backfill did not finish")
	}
	service.backfills.Wait()
	assert.Equal(int32(2), acquisitions.Load())
	assert.False(providerBlocked.Load())
}
