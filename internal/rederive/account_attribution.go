package rederive

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// accountAttributionVersion identifies the account-attribution rules. A bump
// that changes output must also clear account_path for the affected sources,
// because the pass only derives pending rows.
const accountAttributionVersion = "v2"

func init() {
	RegisterAllSourceTypes("account-attribution", accountAttributionVersion, repairAccountAttribution)
}

func repairAccountAttribution(
	ctx context.Context, s *store.Store, sourceID int64, progress func(string),
) (*Summary, error) {
	start := time.Now()
	report := func(sum store.AccountAttributionRepairSummary) {
		if progress != nil {
			progress(fmt.Sprintf("account attribution: %d messages", sum.Scanned))
		}
	}
	sum, err := s.RepairAccountAttributionContext(ctx, sourceID, report)
	return &Summary{
		Duration:                 time.Since(start),
		MessagesScanned:          sum.Scanned,
		MessageMetadataRewritten: sum.Scanned,
		Undecodable:              sum.Undecodable,
	}, err
}
