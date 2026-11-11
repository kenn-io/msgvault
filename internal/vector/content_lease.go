package vector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrContentLeaseLost = errors.New("x content lease lost")

type contentLeaseKey struct{}
type contentLeaseIdentity struct {
	pid          int32
	backendStart time.Time
	key          int64
}

// WithContentLease binds writes to the original PostgreSQL lease session.
func WithContentLease(ctx context.Context, pid int32, backendStart time.Time, key int64) context.Context {
	if pid == 0 {
		return ctx
	}
	return context.WithValue(ctx, contentLeaseKey{}, contentLeaseIdentity{pid, backendStart, key})
}

// CheckContentLease acquires a transaction-level shared lock before validating
// the held session. The transaction lock prevents cleanup until commit.
func CheckContentLease(ctx context.Context, tx *sql.Tx) error {
	identity, ok := ctx.Value(contentLeaseKey{}).(contentLeaseIdentity)
	if !ok {
		return nil
	}
	var acquired bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock_shared($1)`, identity.key).Scan(&acquired); err != nil {
		return fmt.Errorf("acquire X content transaction lease: %w", err)
	}
	if !acquired {
		return ErrContentLeaseLost
	}
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
 SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
 WHERE l.locktype='advisory' AND l.granted AND l.mode='ShareLock' AND l.pid=$1 AND a.backend_start=$2
 AND l.database=(SELECT oid FROM pg_database WHERE datname=current_database())
 AND l.classid=(($3::bigint >> 32) & 4294967295)::oid AND l.objid=($3::bigint & 4294967295)::oid AND l.objsubid=1
)`, identity.pid, identity.backendStart, identity.key).Scan(&valid)
	if err != nil {
		return fmt.Errorf("check X content lease: %w", err)
	}
	if !valid {
		return ErrContentLeaseLost
	}
	return nil
}
