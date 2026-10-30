package api

import (
	"context"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func TestReadClientOptionsRejectAuthentication(t *testing.T) {
	store := NewSessionStore(secureTempDir(t), 123, "hash", 456)
	opts := ReadClientOptions(store, "test-version")
	require.Len(t, opts.Middlewares, 1)
	next := &stubInvoker{}
	invoker := opts.Middlewares[0].Handle(next)
	assert.Error(t, invoker.Invoke(context.Background(), &tg.AuthSendCodeRequest{}, nil))
	assert.Zero(t, next.calls)
}

func TestClientOptionsScopesDataCenterTransfer(t *testing.T) {
	opts := ReadClientOptions(NewSessionStore(secureTempDir(t), 123, "hash", 456), "test")
	next := &stubInvoker{}
	invoker := opts.Middlewares[0].Handle(next)
	require.NoError(t, opts.OnTransfer(context.Background(), nil, func(ctx context.Context) error {
		return invoker.Invoke(ctx, &tg.AuthExportAuthorizationRequest{DCID: 2}, nil)
	}))
	require.Error(t, invoker.Invoke(context.Background(), &tg.AuthExportAuthorizationRequest{DCID: 2}, nil))
	assert.Equal(t, 1, next.calls)
}

func TestReadClientOptionsShareRateLimitAcrossInvokers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	limiter := rate.NewLimiter(rate.Every(time.Hour), 1)
	opts := ReadClientOptionsWithLimiter(NewSessionStore(secureTempDir(t), 123, "hash", 456), "test", limiter)
	first, second := &stubInvoker{}, &stubInvoker{}
	require.NoError(opts.Middlewares[0].Handle(first).Invoke(context.Background(), &tg.MessagesGetHistoryRequest{}, nil))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.Error(opts.Middlewares[0].Handle(second).Invoke(ctx, &tg.MessagesGetHistoryRequest{}, nil))
	assert.Equal(1, first.calls)
	assert.Zero(second.calls)
}
