package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
)

// A refresh owns the policy fence before inspecting messages and draining dirty
// hints. Reply repair must wait for that fence without holding a message row
// that the refresh needs, even when its hint already exists.
func TestConversationPolicyReplyRepairLockOrderPG(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, operation := range []string{"source reply", "message reply", "clear replies"} {
			name := operation
			if scoped {
				name += " scoped"
			}
			t.Run(name, func(t *testing.T) {
				requirements := require.New(t)
				f := newActivityFixture(t)
				st := f.s
				if !st.IsPostgreSQL() {
					t.Skip("PostgreSQL lock-order regression; set MSGVAULT_TEST_DB")
				}
				st.DB().SetMaxOpenConns(4)
				parent := f.message(t, "parent", activityNow, true)
				child := f.message(t, "child", activityNow, false)
				requirements.NoError(st.SetMessageReplyContext(t.Context(), child, parent))
				writer := st
				if scoped {
					writer = scopedSync(t, st, f.source)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				refresh, err := st.DB().BeginTx(ctx, nil)
				requirements.NoError(err)
				defer func() { _ = refresh.Rollback() }()
				requirements.NoError(st.LockPostgresConversationPolicyTx(ctx, refresh, store.ConversationPolicyExclusive))
				// This is the dirty-hint row deleted by a refresh. Holding it
				// makes an unfenced reply stop after locking its message row.
				_, err = refresh.ExecContext(ctx, `UPDATE archive_metadata SET value=value WHERE key LIKE 'conversation_policy_activity_dirty:%'`)
				requirements.NoError(err)
				done := make(chan error, 1)
				go func() {
					switch operation {
					case "source reply":
						done <- writer.SetReplyToContext(ctx, f.source, "child", "parent")
					case "message reply":
						done <- writer.SetMessageReplyContext(ctx, child, parent)
					case "clear replies":
						done <- writer.ClearMessageRepliesContext(ctx, f.source, []int64{child})
					}
				}()
				requirements.Eventually(func() bool {
					var waiting bool
					err := st.DB().QueryRowContext(ctx, `SELECT EXISTS (
						SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
						AND pid<>pg_backend_pid() AND wait_event_type='Lock')`).Scan(&waiting)
					return err == nil && waiting
				}, pgWaitBudget, 10*time.Millisecond, "reply repair must reach its database lock wait")
				var lockedID int64
				err = refresh.QueryRowContext(ctx, st.Rebind(`SELECT id FROM messages WHERE id=? FOR UPDATE NOWAIT`), child).Scan(&lockedID)
				requirements.NoError(err, "reply repair must not lock messages before the policy fence")
				requirements.Equal(child, lockedID)
				requirements.NoError(refresh.Commit())
				requirements.NoError(waitResult(t, done, "reply repair"))
			})
		}
	}
}
