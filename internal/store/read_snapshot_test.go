package store_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// selectOne runs a real statement through the read path the store uses.
func selectOne(ctx context.Context, st *store.Store) error {
	var one int
	return store.ReadDBContext(ctx, st.DB()).QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

func TestRunIndependentReads(t *testing.T) {
	t.Run("overlaps reads outside a snapshot", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		ctx := t.Context()
		started := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
		// Each read waits until the other has started, which only happens
		// when both are in flight at once.
		read := func(self, other int) func(context.Context) error {
			return func(ctx context.Context) error {
				close(started[self])
				select {
				case <-started[other]:
				case <-time.After(10 * time.Second):
					return errors.New("the other read never started")
				}
				return selectOne(ctx, st)
			}
		}
		require.NoError(t, store.RunIndependentReads(ctx, st.DB(), read(0, 1), read(1, 0)))
	})

	t.Run("runs reads in order inside a snapshot", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		st := testutil.NewTestStore(t)
		ctx, release, err := st.BeginReadSnapshotContext(t.Context())
		require.NoError(err)
		t.Cleanup(release)
		var active, maxActive atomic.Int32
		var order []int
		read := func(n int) func(context.Context) error {
			return func(ctx context.Context) error {
				now := active.Add(1)
				defer active.Add(-1)
				if now > maxActive.Load() {
					maxActive.Store(now)
				}
				order = append(order, n)
				return selectOne(ctx, st)
			}
		}
		require.NoError(store.RunIndependentReads(ctx, st.DB(), read(1), read(2), read(3)))
		assert.Equal([]int{1, 2, 3}, order)
		assert.Equal(int32(1), maxActive.Load())
	})

	t.Run("returns the first error in argument order", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		first, second := errors.New("first read"), errors.New("second read")
		secondFailed := make(chan struct{})
		err := store.RunIndependentReads(t.Context(), st.DB(),
			func(context.Context) error {
				select {
				case <-secondFailed:
				case <-time.After(10 * time.Second):
				}
				return first
			},
			func(context.Context) error {
				defer close(secondFailed)
				return second
			},
		)
		require.ErrorIs(t, err, first)
	})
}
