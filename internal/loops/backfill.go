package loops

import (
	"context"
	"time"
)

func (d *Deriver) backfill(ctx context.Context, now time.Time, result *RunResult) error {
	batch, err := d.st.BackfillLoopSignalsContext(ctx, d.cfg.SignalsBackfillBatch, now)
	if err != nil {
		return err
	}
	result.BackfillScanned = batch.Scanned
	result.BackfillParsed = batch.Parsed
	result.BackfillMissing = batch.Missing
	result.BackfillEncrypted = batch.Encrypted
	result.BackfillUnsupported = batch.Unsupported
	result.BackfillTooLarge = batch.TooLarge
	result.BackfillInvalid = batch.Invalid
	return nil
}
