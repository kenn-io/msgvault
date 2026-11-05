package jobctx

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPreemptionRequestVisibleThroughDerivedContexts(t *testing.T) {
	assert := assert.New(t)
	assert.False(PreemptionRequested(context.Background()), "plain context is never preempted")
	assert.Nil(PreemptionSignal(context.Background()), "plain contexts have no preemption signal")

	ctx, request := WithPreemption(context.Background())
	signal := PreemptionSignal(ctx)
	assert.NotNil(signal)
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	assert.False(PreemptionRequested(child))
	select {
	case <-signal:
		assert.Fail("signal closed before preemption was requested")
	default:
	}

	request()
	request()
	select {
	case <-signal:
	default:
		assert.Fail("signal remained open after preemption was requested")
	}
	assert.True(PreemptionRequested(child), "request reaches derived contexts")
	assert.NoError(child.Err(), "preemption does not cancel")
	assert.NoError(context.WithoutCancel(ctx).Err())
	assert.True(PreemptionRequested(context.WithoutCancel(ctx)))
}
