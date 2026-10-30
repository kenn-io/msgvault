package api

import (
	"context"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

type stubInvoker struct {
	calls int
	err   error
}

func (s *stubInvoker) Invoke(_ context.Context, _ bin.Encoder, _ bin.Decoder) error {
	s.calls++
	return s.err
}

func TestInvokerReadOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	underlying := &stubInvoker{}
	invoker := NewInvoker(underlying, rate.Inf, 1, time.Minute)
	require.NoError(invoker.Invoke(context.Background(), &tg.MessagesGetHistoryRequest{}, nil))
	require.NoError(invoker.Invoke(context.Background(), &tg.ChannelsGetFullChannelRequest{}, nil))
	require.NoError(invoker.Invoke(context.Background(), &tg.AccountFinishTakeoutSessionRequest{}, nil))
	assert.Error(invoker.Invoke(context.Background(), &tg.MessagesSendMessageRequest{}, nil))
	assert.Error(invoker.Invoke(context.Background(), &tg.InvokeWithTakeoutRequest{Query: &tg.MessagesSendMessageRequest{}}, nil))
	require.NoError(invoker.Invoke(context.Background(), &tg.InvokeWithTakeoutRequest{Query: &tg.AccountFinishTakeoutSessionRequest{}}, nil))
	require.NoError(invoker.Invoke(context.Background(), &tg.InvokeWithTakeoutRequest{Query: &tg.MessagesGetHistoryRequest{}}, nil))
	require.NoError(invoker.Invoke(context.Background(), &tg.InvokeWithTakeoutRequest{Query: &tg.InvokeWithMessagesRangeRequest{
		Range: tg.MessageRange{MinID: 1, MaxID: 10}, Query: &tg.MessagesGetHistoryRequest{},
	}}, nil))
	assert.Equal(6, underlying.calls)
}

func TestInvokerFloodWaitCeilingAndCancellation(t *testing.T) {
	underlying := &stubInvoker{err: tgerr.New(420, "FLOOD_WAIT_10")}
	invoker := NewInvoker(underlying, rate.Inf, 1, time.Second)
	require.Error(t, invoker.Invoke(context.Background(), &tg.MessagesGetHistoryRequest{}, nil))
	assert.Equal(t, 1, underlying.calls)
	underlying.err = tgerr.New(420, "FLOOD_WAIT_1")
	invoker = NewInvoker(underlying, rate.Inf, 1, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, invoker.Invoke(ctx, &tg.MessagesGetHistoryRequest{}, nil), context.Canceled)
}

type floodOnceInvoker struct{ calls int }

func (s *floodOnceInvoker) Invoke(_ context.Context, _ bin.Encoder, _ bin.Decoder) error {
	s.calls++
	if s.calls == 1 {
		return tgerr.New(420, "FLOOD_WAIT_0")
	}
	return nil
}

func TestInvokerRetriesFloodWait(t *testing.T) {
	underlying := &floodOnceInvoker{}
	invoker := NewInvoker(underlying, rate.Inf, 1, time.Minute)
	require.NoError(t, invoker.Invoke(context.Background(), &tg.MessagesGetHistoryRequest{}, nil))
	assert.Equal(t, 2, underlying.calls)
}

func TestInvokerRejectsNilAndExcessiveWrappers(t *testing.T) {
	underlying := &stubInvoker{}
	invoker := NewInvoker(underlying, rate.Inf, 1, time.Minute)
	var history *tg.MessagesGetHistoryRequest
	assert.Error(t, invoker.Invoke(context.Background(), history, nil))
	var request bin.Object = &tg.MessagesGetHistoryRequest{}
	for range 100 {
		request = &tg.InvokeWithTakeoutRequest{Query: request}
	}
	assert.Error(t, invoker.Invoke(context.Background(), request, nil))
	assert.Zero(t, underlying.calls)
}

func TestInvokerRejectsOverflowingFloodWait(t *testing.T) {
	underlying := &stubInvoker{err: tgerr.New(420, "FLOOD_WAIT_9223372037")}
	invoker := NewInvoker(underlying, rate.Inf, 1, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := invoker.Invoke(ctx, &tg.MessagesGetHistoryRequest{}, nil)
	require.Error(t, err)
	assert.NotErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, underlying.calls)
}

func TestReadInvokerRejectsAuthenticationRequests(t *testing.T) {
	underlying := &stubInvoker{}
	invoker := NewReadInvoker(underlying, rate.Inf, 1, time.Minute)
	require.Error(t, invoker.Invoke(context.Background(), &tg.AuthSendCodeRequest{}, nil))
	require.NoError(t, invoker.Invoke(context.Background(), &tg.MessagesGetHistoryRequest{}, nil))
	assert.Equal(t, 1, underlying.calls)
}

func TestReadInvokerAllowsOnlyScopedDataCenterTransfer(t *testing.T) {
	require := require.New(t)

	underlying := &stubInvoker{}
	invoker := NewReadInvoker(underlying, rate.Inf, 1, time.Minute)
	request := &tg.AuthExportAuthorizationRequest{DCID: 2}
	require.Error(invoker.Invoke(context.Background(), request, nil))
	transfer := context.WithValue(context.Background(), transferContextKey{}, true)
	require.NoError(invoker.Invoke(transfer, request, nil))
	require.Error(invoker.Invoke(transfer, &tg.AuthSendCodeRequest{}, nil))
	require.Error(invoker.Invoke(transfer, &tg.InvokeWithTakeoutRequest{Query: request}, nil))
	for _, method := range []bin.Encoder{&tg.HelpGetCDNConfigRequest{}, &tg.UploadGetFileHashesRequest{}, &tg.UploadGetCDNFileRequest{}, &tg.UploadGetCDNFileHashesRequest{}, &tg.UploadReuploadCDNFileRequest{}} {
		require.NoError(invoker.Invoke(context.Background(), method, nil))
	}
	assert.Equal(t, 6, underlying.calls)
}

func TestReadInvokerAllowsMessageRefresh(t *testing.T) {
	underlying := &stubInvoker{}
	invoker := NewReadInvoker(underlying, rate.Inf, 1, time.Minute)
	require.NoError(t, invoker.Invoke(context.Background(), &tg.MessagesGetMessagesRequest{}, nil))
	require.NoError(t, invoker.Invoke(context.Background(), &tg.ChannelsGetMessagesRequest{}, nil))
	assert.Equal(t, 2, underlying.calls)
}
