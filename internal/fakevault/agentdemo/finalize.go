package agentdemo

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
)

func (b *builder) finalize(ctx context.Context) error {
	for _, handle := range slices.Sorted(maps.Keys(b.m.Sources)) {
		if err := b.st.RecomputeConversationStatsContext(ctx, b.source(handle)); err != nil {
			return err
		}
	}
	if _, err := b.st.DB().ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return err
	}
	digest, err := Digest(ctx, b.st.DB())
	if err != nil {
		return err
	}
	b.m.Digest = digest
	// Keep untyped oracle values identical before and after a JSON round trip.
	data, err := json.Marshal(b.m.Expect)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &b.m.Expect); err != nil {
		return err
	}
	return nil
}
