package jobctx

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPreemptionRequestVisibleThroughDerivedContexts(t *testing.T) {
	assert := assert.New(t)
	assert.False(PreemptionRequested(context.Background()), "plain context is never preempted")

	ctx, request := WithPreemption(context.Background())
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	assert.False(PreemptionRequested(child))

	request()
	request()
	assert.True(PreemptionRequested(child), "request reaches derived contexts")
	assert.NoError(child.Err(), "preemption does not cancel")
	assert.NoError(context.WithoutCancel(ctx).Err())
	assert.True(PreemptionRequested(context.WithoutCancel(ctx)))
}
