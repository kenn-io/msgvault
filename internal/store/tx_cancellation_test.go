package store

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransactionBeginFailureLogLevel(t *testing.T) {
	for _, snapshot := range []bool{false, true} {
		name := "write"
		if snapshot {
			name = "read snapshot"
		}
		t.Run(name, func(t *testing.T) {
			for _, failure := range []string{"canceled", "deadline", "closed database"} {
				t.Run(failure, func(t *testing.T) {
					assert, require := assert.New(t), require.New(t)
					db := openLoggedMem(t)
					st := &Store{db: db}
					ctx := WithRequestID(context.Background(), "test-request")
					wantLevel := slog.LevelWarn.String()
					var wantErr error
					switch failure {
					case "canceled":
						var cancel context.CancelFunc
						ctx, cancel = context.WithCancel(ctx)
						cancel()
						wantErr = context.Canceled
						wantLevel = slog.LevelDebug.String()
					case "deadline":
						var cancel context.CancelFunc
						ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Hour))
						defer cancel()
						wantErr = context.DeadlineExceeded
					case "closed database":
						require.NoError(db.Close())
					}
					logs := captureSlog(t)
					var err error
					if snapshot {
						_, release, beginErr := st.BeginReadSnapshotContext(ctx)
						err = beginErr
						assert.Nil(release)
					} else {
						called := false
						err = st.withTxContext(ctx, func(*loggedTx) error {
							called = true
							return nil
						})
						assert.False(called, "a failed begin must not run the transaction callback")
					}
					require.Error(err)
					if wantErr != nil {
						require.ErrorIs(err, wantErr)
					}
					record := findLogLineByMsg(t, logs, "sql tx begin failed")
					require.NotNil(record)
					assert.Equal(wantLevel, record["level"])
					assert.NotEmpty(record["error"])
					if snapshot {
						assert.Equal("test-request", record["request_id"])
						assert.Contains(record, "duration_ms")
					}
				})
			}
		})
	}
}
