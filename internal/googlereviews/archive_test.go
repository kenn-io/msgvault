package googlereviews

import (
	"context"
	"errors"
	"sync"
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
		a.state = BackfillState{Target: target, Location: location, Status: "running", RunID: 1, StartedAt: timePointerForTest(time.Now())}
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

func (a *archiveTestStore) GoogleReviewsBackfillState(_ context.Context, target string) (BackfillState, error) {
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
	require.ErrorIs(t, err, ErrDenied)
	result, err := service.Execute(t.Context(), "owner", Request{Action: "backfill", Target: "shop"})
	require.NoError(t, err)
	require.NotNil(t, result.ArchiveSync)
	assert.Equal(t, "running", result.ArchiveSync.Status)

	select {
	case <-archive.done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "backfill did not reach an observable completed state")
	}
	status, err := service.Execute(t.Context(), "owner", Request{Action: "archive_status", Target: "shop"})
	require.NoError(t, err)
	require.NotNil(t, status.ArchiveSync)
	assert.Equal(t, "api_exhausted", status.ArchiveSync.Status)
	assert.Equal(t, int64(2), status.ArchiveSync.PagesFetched)
	assert.Equal(t, int64(2), status.ArchiveSync.ReviewsFetched)
	assert.Equal(t, backfillCoverageNote, status.ArchiveSync.CoverageNote)
	assert.Equal(t, []int{50, 50}, provider.pageSizes)
	assert.Zero(t, provider.puts)
}

func TestBackfillPersistsOnlyClassifiedProviderFailure(t *testing.T) {
	const location = "accounts/a/locations/l"
	archive := &archiveTestStore{}
	provider := &archiveTestProvider{err: ErrQuota}
	target := Target{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Location: location, Enabled: true, OwnProject: true}
	service := NewService(Options{
		Archive:  archive,
		Targets:  func(context.Context) ([]Target, error) { return []Target{target}, nil },
		Provider: func(context.Context, Target) (Provider, string, error) { return provider, "synthetic-binding", nil },
	})
	t.Cleanup(service.Close)
	_, err := service.Execute(t.Context(), "owner", Request{Action: "backfill", Target: "shop"})
	require.NoError(t, err)

	select {
	case <-archive.done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "backfill failure was not persisted")
	}
	status, statusErr := service.Execute(t.Context(), "owner", Request{Action: "archive_status", Target: "shop"})
	require.NoError(t, statusErr)
	require.NotNil(t, status.ArchiveSync)
	assert.Equal(t, "partial", status.ArchiveSync.Status)
	assert.Equal(t, "quota", status.ArchiveSync.LastErrorCode)
	assert.Zero(t, provider.puts)
}

func TestConcurrentBackfillRequestsShareOneWorker(t *testing.T) {
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
	require.NoError(t, err)
	select {
	case <-provider.listEntered:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "backfill worker did not start its provider read")
	}

	result, err := service.Execute(t.Context(), "owner", Request{Action: "backfill", Target: "shop"})
	require.NoError(t, err)
	require.NotNil(t, result.ArchiveSync)
	assert.Equal(t, "running", result.ArchiveSync.Status)
	assert.Equal(t, 1, provider.calls)
	archive.mu.Lock()
	assert.Equal(t, 1, archive.begins)
	archive.mu.Unlock()

	close(provider.listRelease)
	select {
	case <-archive.done:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "backfill worker did not finish its observable archive update")
	}
	assert.Zero(t, provider.puts)
}

func timePointerForTest(value time.Time) *time.Time { return &value }

var _ Archive = (*archiveTestStore)(nil)
var _ Provider = (*archiveTestProvider)(nil)

func TestInvalidateSettlesBackfillAndRejectsStaleResume(t *testing.T) {
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
	require.NoError(t, err)
	select {
	case <-provider.listEntered:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "provider read did not start")
	}
	first, err := service.Execute(t.Context(), "owner", Request{Action: "invalidate_target", Target: "shop"})
	require.NoError(t, err)
	require.NotEmpty(t, first.CredentialChangeToken)
	archive.mu.Lock()
	assert.Equal(t, "partial", archive.state.Status)
	assert.Empty(t, archive.reviews)
	archive.mu.Unlock()
	_, err = service.Execute(t.Context(), "owner", Request{Action: "backfill", Target: "alias"})
	assert.ErrorIs(t, err, ErrCredentials)
	second, err := service.Execute(t.Context(), "owner", Request{Action: "invalidate_target", Target: "shop"})
	require.NoError(t, err)
	_, err = service.Execute(t.Context(), "owner", Request{Action: "resume_target", Target: "shop", ConfirmationToken: first.CredentialChangeToken})
	assert.ErrorIs(t, err, ErrConflict)
	_, err = service.Execute(t.Context(), "owner", Request{Action: "resume_target", Target: "shop", ConfirmationToken: second.CredentialChangeToken})
	require.NoError(t, err)
	close(provider.listRelease)
	_, err = service.Execute(t.Context(), "owner", Request{Action: "backfill", Target: "alias"})
	assert.NoError(t, err)
}

func TestBackfillStopsWhenTargetDisabledBetweenPages(t *testing.T) {
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
	require.NoError(t, err)
	select {
	case <-archive.done:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "configuration change was not recorded")
	}
	archive.mu.Lock()
	assert.Equal(t, "configuration_changed", archive.state.LastErrorCode)
	assert.Equal(t, int64(1), archive.state.PagesFetched)
	archive.mu.Unlock()
	assert.Equal(t, 1, provider.calls)
}
