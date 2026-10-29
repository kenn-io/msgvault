package api

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
