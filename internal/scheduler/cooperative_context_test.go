package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCooperativeJobContextStopsWithCallerOrScheduler(t *testing.T) {
	for _, stopCaller := range []bool{true, false} {
		t.Run(map[bool]string{true: "caller", false: "scheduler"}[stopCaller], func(t *testing.T) {
			s := New(nil)
			caller, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, stop := s.CooperativeJobContext(caller, "test build")
			defer stop()
			if stopCaller {
				cancel()
			} else {
				<-s.Stop().Done()
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				require.FailNow(t, "context did not stop")
			}
			assert.ErrorIs(t, ctx.Err(), context.Canceled)
			if stopCaller {
				<-s.Stop().Done()
			}
		})
	}
}
