package googlereviews

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

type memoryProvider struct {
	mu        sync.Mutex
	review    Review
	puts      int
	putError  error
	unchanged bool
}

func (p *memoryProvider) List(context.Context, string, string, int) (Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Page{Reviews: []Review{p.review}}, nil
}
func (p *memoryProvider) Get(context.Context, string) (Review, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v := p.review
	if v.ReviewReply != nil {
		reply := *v.ReviewReply
		v.ReviewReply = &reply
	}
	return v, nil
}
func (p *memoryProvider) UpdateReply(_ context.Context, _ string, comment string) (Reply, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.puts++
	r := Reply{Comment: comment, UpdateTime: "changed"}
	if !p.unchanged {
		p.review.ReviewReply = &r
	}
	return r, p.putError
}

type blockingGetProvider struct {
	Provider

	calls     atomic.Int32
	blockCall int32
	started   chan struct{}
	release   chan struct{}
}

func (p *blockingGetProvider) Get(ctx context.Context, name string) (Review, error) {
	if p.calls.Add(1) == p.blockCall {
		close(p.started)
		select {
		case <-p.release:
		case <-ctx.Done():
			return Review{}, ctx.Err()
		}
	}
	return p.Provider.Get(ctx, name)
}

func serviceFixture(t *testing.T) (*Service, *memoryProvider, *Target, *string) {
	t.Helper()
	p := &memoryProvider{review: Review{Name: "accounts/a/locations/l/reviews/r", ReviewID: "r", Comment: "Review", UpdateTime: "old"}}
	target := &Target{Name: "shop", Provider: "google", Email: "owner@example.com", OAuthApp: "business", Location: "accounts/a/locations/l", Enabled: true, RepliesEnabled: true, OwnProject: true}
	binding := new(string)
	*binding = "checked credential"
	s := NewService(Options{Targets: func(context.Context) ([]Target, error) {
		return []Target{*target, {Name: "alias", Provider: target.Provider, Email: target.Email, OAuthApp: target.OAuthApp, Location: target.Location, Enabled: true, RepliesEnabled: true, OwnProject: true}}, nil
	}, Provider: func(context.Context, Target) (Provider, string, error) { return p, *binding, nil }})
	t.Cleanup(s.Close)
	return s, p, target, binding
}
func issue(t *testing.T, s *Service, target string) *Preview {
	t.Helper()
	r, err := s.Execute(t.Context(), "owner", Request{Action: "preview_reply", Target: target, ReviewID: "r", Comment: "Thank you"})
	require.NoError(t, err)
	require.NotNil(t, r.Preview)
	return r.Preview
}

func TestUnknownSubmissionInvalidatesDelayedPreviewRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, provider, _, _ := serviceFixture(t)
	delayed := &blockingGetProvider{
		Provider:  provider,
		blockCall: 2,
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	s.options.Provider = func(context.Context, Target) (Provider, string, error) {
		return delayed, "checked credential", nil
	}
	first := issue(t, s, "shop")
	type previewOutcome struct {
		result *Result
		err    error
	}
	previewDone := make(chan previewOutcome, 1)
	go func() {
		result, err := s.Execute(t.Context(), "owner", Request{Action: "preview_reply", Target: "alias", ReviewID: "r", Comment: "Thank you"})
		previewDone <- previewOutcome{result: result, err: err}
	}()
	require.Eventually(func() bool {
		select {
		case <-delayed.started:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)

	provider.mu.Lock()
	provider.putError = ErrUnknown
	provider.unchanged = true
	provider.mu.Unlock()
	result, err := s.Execute(t.Context(), "owner", Request{Action: "confirm_reply", ConfirmationToken: first.ConfirmationToken, Confirmed: true})
	require.NoError(err)
	assert.Equal("unknown", result.Receipt.Status)
	close(delayed.release)
	late := <-previewDone
	require.ErrorIs(late.err, ErrConflict)
	assert.Nil(late.result)
	assert.Empty(s.pending)
	receipt, err := s.Execute(t.Context(), "owner", Request{Action: "receipt", ReceiptID: first.ReceiptID})
	require.NoError(err)
	assert.Equal("unknown", receipt.Receipt.Status)
	assert.Equal(1, provider.puts)
}

func TestNoWriteWithoutSpecificConsent(t *testing.T) {
	s, p, _, _ := serviceFixture(t)
	hegel.Test(t, func(ht *hegel.T) {
		token := hegel.Draw(ht, hegel.FromRegex(`[a-zA-Z0-9_-]{1,256}`, true))
		_, err := s.Execute(t.Context(), "owner", Request{Action: "confirm_reply", ConfirmationToken: token, Confirmed: true})
		require.ErrorIs(ht, err, ErrConflict)
		assert.Zero(ht, p.puts)
	})
	assert := assert.New(t)
	require := require.New(t)
	preview := issue(t, s, "shop")
	_, err := s.Execute(t.Context(), "owner", Request{Action: "confirm_reply", ConfirmationToken: preview.ConfirmationToken})
	require.Error(err)
	_, err = s.Execute(t.Context(), "different owner", Request{Action: "confirm_reply", ConfirmationToken: preview.ConfirmationToken, Confirmed: true})
	require.ErrorIs(err, ErrDenied)
	assert.Zero(p.puts)
	r, err := s.Execute(t.Context(), "owner", Request{Action: "confirm_reply", ConfirmationToken: preview.ConfirmationToken, Confirmed: true})
	require.NoError(err)
	assert.Equal("submitted", r.Receipt.Status)
	_, err = s.Execute(t.Context(), "owner", Request{Action: "confirm_reply", ConfirmationToken: preview.ConfirmationToken, Confirmed: true})
	require.Error(err)
	assert.Equal(1, p.puts)
}
func TestPreviewBinding(t *testing.T) {
	for _, change := range []string{"credential", "config", "review", "reply"} {
		t.Run(change, func(t *testing.T) {
			s, p, target, binding := serviceFixture(t)
			preview := issue(t, s, "shop")
			switch change {
			case "credential":
				*binding = "new credential"
			case "config":
				target.OAuthApp = "newapp"
			case "review":
				p.review.Comment = "changed"
			case "reply":
				p.review.ReviewReply = &Reply{Comment: "already replied"}
			}
			_, err := s.Execute(t.Context(), "owner", Request{Action: "confirm_reply", ConfirmationToken: preview.ConfirmationToken, Confirmed: true})
			require.ErrorIs(t, err, ErrConflict)
			assert.Zero(t, p.puts)
		})
	}
}
func TestCanonicalReviewConcurrentAliases(t *testing.T) {
	s, p, _, _ := serviceFixture(t)
	a := issue(t, s, "shop")
	b := issue(t, s, "alias")
	var succeeded atomic.Int32
	var wg sync.WaitGroup
	for _, v := range []*Preview{a, b} {
		wg.Go(func() {
			_, err := s.Execute(t.Context(), "owner", Request{Action: "confirm_reply", ConfirmationToken: v.ConfirmationToken, Confirmed: true})
			if err == nil {
				succeeded.Add(1)
			} else {
				assert.ErrorIs(t, err, ErrConflict)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), succeeded.Load())
	assert.Equal(t, 1, p.puts)
}
func TestUnknownOutcomeAndReceiptIsolation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, p, _, _ := serviceFixture(t)
	p.putError = ErrUnknown
	p.unchanged = true
	a := issue(t, s, "shop")
	b := issue(t, s, "alias")
	result, err := s.Execute(t.Context(), "owner", Request{Action: "confirm_reply", ConfirmationToken: a.ConfirmationToken, Confirmed: true})
	require.NoError(err)
	assert.Equal("unknown", result.Receipt.Status)
	receipt, err := s.Execute(t.Context(), "owner", Request{Action: "receipt", ReceiptID: a.ReceiptID})
	require.NoError(err)
	assert.Equal("unknown", receipt.Receipt.Status)
	_, err = s.Execute(t.Context(), "stranger", Request{Action: "receipt", ReceiptID: a.ReceiptID})
	require.ErrorIs(err, ErrDenied)
	_, err = s.Execute(t.Context(), "owner", Request{Action: "confirm_reply", ConfirmationToken: b.ConfirmationToken, Confirmed: true})
	require.ErrorIs(err, ErrConflict)
	assert.Equal(1, p.puts)
}
func TestExpiryCapacityAssistanceAndClose(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, p, _, _ := serviceFixture(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	assisted, err := s.Execute(t.Context(), "owner", Request{Action: "assist_reply", Target: "shop", ReviewID: "r", Comment: "Thank you"})
	require.NoError(err)
	assert.Empty(assisted.Preview.ConfirmationToken)
	assert.Empty(assisted.Preview.ReceiptID)
	assert.Empty(s.pending)
	preview := issue(t, s, "shop")
	now = now.Add(6 * time.Minute)
	s.expire()
	_, err = s.Execute(t.Context(), "owner", Request{Action: "confirm_reply", ConfirmationToken: preview.ConfirmationToken, Confirmed: true})
	require.ErrorIs(err, ErrConflict)
	assert.Empty(s.pending)
	now = now.Add(5 * time.Minute)
	s.expire()
	assert.Empty(s.receipts)
	for range 128 {
		issue(t, s, "shop")
	}
	_, err = s.Execute(t.Context(), "owner", Request{Action: "preview_reply", Target: "shop", ReviewID: "r", Comment: "Thanks"})
	require.ErrorIs(err, ErrCapacity)
	assert.Zero(p.puts)
	s.Close()
	s.Close()
	assert.Empty(s.pending)
	assert.Empty(s.receipts)
	_, err = s.Execute(t.Context(), "owner", Request{Action: "capabilities"})
	require.ErrorIs(err, ErrUnavailable)
}

func TestSetupInvalidationRevokesAliasPreviews(t *testing.T) {
	for _, namedPending := range []bool{false, true} {
		for _, scope := range []string{"same location", "shared credential", "disabled named target"} {
			t.Run(fmt.Sprintf("%s/named pending=%t", scope, namedPending), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				s, p, target, _ := serviceFixture(t)
				alias := *target
				alias.Name = "alias"
				if scope == "same location" {
					alias.OAuthApp = "alias-business"
					alias.Email = "alias@example.test"
				}
				if scope == "shared credential" {
					target.Location = "accounts/a/locations/other"
				}
				s.options.Targets = func(context.Context) ([]Target, error) { return []Target{*target, alias}, nil }
				var previews []*Preview
				if namedPending {
					p.review.Name = target.Location + "/reviews/r"
					previews = append(previews, issue(t, s, "shop"))
				}
				p.review.Name = alias.Location + "/reviews/r"
				previews = append(previews, issue(t, s, "alias"))
				if scope == "disabled named target" {
					target.Enabled = false
				}
				providerCalls := 0
				s.options.Provider = func(context.Context, Target) (Provider, string, error) {
					providerCalls++
					return nil, "", ErrUnavailable
				}
				_, err := s.Execute(t.Context(), "", Request{Action: "invalidate_target", Target: "shop"})
				require.ErrorIs(err, ErrDenied)
				_, err = s.Execute(t.Context(), "owner", Request{Action: "invalidate_target", Target: "shop"})
				require.NoError(err)
				for _, preview := range previews {
					receipt, err := s.Execute(t.Context(), "owner", Request{Action: "receipt", ReceiptID: preview.ReceiptID})
					require.NoError(err)
					assert.Equal("failed", receipt.Receipt.Status)
					_, err = s.Execute(t.Context(), "owner", Request{Action: "confirm_reply", ConfirmationToken: preview.ConfirmationToken, Confirmed: true})
					require.ErrorIs(err, ErrConflict)
				}
				assert.Zero(providerCalls)
				assert.Zero(p.puts)
			})
		}
	}
}
