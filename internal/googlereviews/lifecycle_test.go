package googlereviews

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type controlledProvider struct {
	*memoryProvider

	mu2        sync.Mutex
	blockGet   bool
	getStarted chan struct{}
	releaseGet chan struct{}
	afterPut   func()
}

func (p *controlledProvider) Get(ctx context.Context, name string) (Review, error) {
	p.mu2.Lock()
	block := p.blockGet
	p.mu2.Unlock()
	if block {
		p.getStarted <- struct{}{}
		<-p.releaseGet
	}
	return p.memoryProvider.Get(ctx, name)
}
func (p *controlledProvider) UpdateReply(ctx context.Context, name, comment string) (Reply, error) {
	reply, err := p.memoryProvider.UpdateReply(ctx, name, comment)
	if p.afterPut != nil {
		p.afterPut()
	}
	return reply, err
}
func controlledFixture(t *testing.T) (*Service, *controlledProvider) {
	t.Helper()
	s, p, _, binding := serviceFixture(t)
	controlled := &controlledProvider{memoryProvider: p, getStarted: make(chan struct{}, 2), releaseGet: make(chan struct{})}
	s.options.Provider = func(context.Context, Target) (Provider, string, error) { return controlled, *binding, nil }
	return s, controlled
}
func TestClosePreventsLateProviderWrite(t *testing.T) {
	s, p := controlledFixture(t)
	preview := issue(t, s, "shop")
	p.blockGet = true
	confirmDone := make(chan error, 1)
	go func() {
		_, err := s.Execute(t.Context(), "owner", Request{Action: "confirm_reply", ConfirmationToken: preview.ConfirmationToken, Confirmed: true})
		confirmDone <- err
	}()
	<-p.getStarted
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	<-s.stop
	close(p.releaseGet)
	<-closed
	require.Error(t, <-confirmDone)
	assert.Zero(t, p.puts)
}
func TestQueuedCancellationConsumesOnlyUnclaimedOwnerToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, _ := controlledFixture(t)
	preview := issue(t, s, "shop")
	s.writeGate <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.Execute(ctx, "stranger", Request{Action: "confirm_reply", ConfirmationToken: preview.ConfirmationToken, Confirmed: true})
	require.Error(err)
	receipt, err := s.Execute(t.Context(), "owner", Request{Action: "receipt", ReceiptID: preview.ReceiptID})
	require.NoError(err)
	assert.Equal("pending_confirmation", receipt.Receipt.Status)
	_, err = s.Execute(ctx, "owner", Request{Action: "confirm_reply", ConfirmationToken: preview.ConfirmationToken, Confirmed: true})
	require.Error(err)
	receipt, err = s.Execute(t.Context(), "owner", Request{Action: "receipt", ReceiptID: preview.ReceiptID})
	require.NoError(err)
	assert.Equal("failed", receipt.Receipt.Status)
	<-s.writeGate
	_, err = s.Execute(t.Context(), "owner", Request{Action: "confirm_reply", ConfirmationToken: preview.ConfirmationToken, Confirmed: true})
	require.ErrorIs(err, ErrConflict)
}
func TestReturnedPreviewCannotChangeConsentSnapshot(t *testing.T) {
	s, p, _, _ := serviceFixture(t)
	p.review.ReviewReply = &Reply{Comment: "old"}
	preview := issue(t, s, "shop")
	preview.Review.ReviewReply.Comment = "new"
	p.review.ReviewReply = &Reply{Comment: "new"}
	_, err := s.Execute(t.Context(), "owner", Request{Action: "confirm_reply", ConfirmationToken: preview.ConfirmationToken, Confirmed: true})
	require.ErrorIs(t, err, ErrConflict)
	assert.Zero(t, p.puts)
}

func TestCancellationAtFinalReadAndAcceptedWrite(t *testing.T) {
	for _, phase := range []string{"final read", "accepted write"} {
		t.Run(phase, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s, p := controlledFixture(t)
			preview := issue(t, s, "shop")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if phase == "final read" {
				p.blockGet = true
			} else {
				p.afterPut = cancel
			}
			done := make(chan *Result, 1)
			go func() {
				r, _ := s.Execute(ctx, "owner", Request{Action: "confirm_reply", ConfirmationToken: preview.ConfirmationToken, Confirmed: true})
				done <- r
			}()
			if phase == "final read" {
				<-p.getStarted
				cancel()
				close(p.releaseGet)
			}
			<-done
			require.ErrorIs(ctx.Err(), context.Canceled)
			result, err := s.Execute(t.Context(), "owner", Request{Action: "receipt", ReceiptID: preview.ReceiptID})
			require.NoError(err)
			if phase == "final read" {
				assert.Equal("failed", result.Receipt.Status)
				assert.Zero(p.puts)
			} else {
				assert.Equal("unknown", result.Receipt.Status)
				assert.Equal(1, p.puts)
			}
		})
	}
}
