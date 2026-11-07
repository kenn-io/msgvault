package rederive

import (
	"context"
	"fmt"
	"strings"
)

const emailCheckpointRows = 128
const emailCursor = "#c"

// restore accepts only a cursor whose failed-row coverage can be reconstructed.
// Legacy interrupted walks without a cursor safely restart from the beginning.
func (w *emailWalk) restore(ctx context.Context) error {
	if !w.resume {
		return w.s.ForgetMigrationsContext(ctx, w.base)
	}
	if err := w.restoreCursor(ctx); err != nil {
		return err
	}
	if w.cursor == "" {
		return w.s.ForgetMigrationsContext(ctx, w.base)
	}
	prefix := w.base + emailRetrySlot
	for key := ""; ; {
		next, err := w.s.NextMigrationKeyContext(ctx, prefix, key)
		if err != nil {
			return err
		}
		if next == "" {
			break
		}
		if _, ok := parseEmailRetryID(strings.TrimPrefix(next, prefix)); !ok || w.slots == emailRetrySlots {
			return w.malformed(ctx, next)
		}
		w.slots++
		key = next
	}
	var err error
	w.rangeKey, w.lo, w.hi, err = w.readRange(ctx)
	return err
}

func (w *emailWalk) restoreCursor(ctx context.Context) error {
	prefix := w.base + emailCursor
	key, err := w.s.NextMigrationKeyContext(ctx, prefix, "")
	if err != nil {
		return err
	}
	if key == "" {
		return nil // Finished legacy walks can have retry markers without a cursor.
	}
	source, after, ok := strings.Cut(strings.TrimPrefix(key, prefix), "-")
	sourceID, validSource := parseEmailRetryID(source)
	afterID, validAfter := parseEmailRetryID(after)
	if !ok || !validSource || !validAfter || sourceID != w.sourceID {
		return w.malformed(ctx, key)
	}
	if next, err := w.s.NextMigrationKeyContext(ctx, prefix, key); err != nil {
		return err
	} else if next != "" {
		return w.malformed(ctx, next)
	}
	w.cursor, w.afterID, w.savedID = key, afterID, afterID
	return nil
}

func (w *emailWalk) readRange(ctx context.Context) (key string, first, last int64, err error) {
	prefix := w.base + emailRetryRange
	key, err = w.s.NextMigrationKeyContext(ctx, prefix, "")
	if err != nil || key == "" {
		return key, 0, 0, err
	}
	lo, hi, ok := strings.Cut(strings.TrimPrefix(key, prefix), "-")
	first, firstOK := parseEmailRetryID(lo)
	last, lastOK := parseEmailRetryID(hi)
	if !ok || !firstOK || !lastOK || first > last {
		return key, 0, 0, w.malformed(ctx, key)
	}
	if next, err := w.s.NextMigrationKeyContext(ctx, prefix, key); err != nil {
		return key, 0, 0, err
	} else if next != "" {
		return key, 0, 0, w.malformed(ctx, next)
	}
	return key, first, last, nil
}

// checkpoint persists overflow coverage before the cursor crosses those rows.
// A crash between the two atomic replacements replays rows with their failure
// markers intact. One cursor and one overflow range bound the ledger state.
func (w *emailWalk) checkpoint(ctx context.Context) error {
	if w.afterID == w.savedID {
		return nil
	}
	if w.lo > 0 {
		key := emailRetryRangeKey(w.base, w.lo, w.hi)
		if key != w.rangeKey {
			if err := w.s.ReplaceMigrationContext(ctx, w.rangeKey, key); err != nil {
				return err
			}
			w.rangeKey = key
		}
	}
	key := fmt.Sprintf("%s%s%016x-%016x", w.base, emailCursor, w.sourceID, w.afterID)
	if err := w.s.ReplaceMigrationContext(ctx, w.cursor, key); err != nil {
		return err
	}
	w.cursor, w.savedID = key, w.afterID
	return nil
}
