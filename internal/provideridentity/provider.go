// Package provideridentity maps external provider inventory into the shared
// identity evidence contract without giving provider clients store access.
package provideridentity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fastmail"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/store"
)

// Inventory is the credential-bearing provider read seam. Tests inject an
// in-memory implementation; the production implementation is the JMAP client.
type Inventory interface {
	ListIdentityRecords(ctx context.Context) ([]fastmail.Record, error)
}

// Factory binds one provider token to an inventory client.
type Factory func(apiToken string) Inventory

// Store is the complete source lookup and bounded identity-write surface used
// by automatic provider refresh.
type Store interface {
	config.FastmailSourceStore
	identityops.ExternalEvidenceStore
	RecordProviderIdentityRefreshOutcomeContext(ctx context.Context, sourceID int64, refreshErr error) error
	ProviderIdentityRefreshStateContext(ctx context.Context, sourceID int64) (store.ProviderIdentityRefreshState, bool, error)
}

// RefreshStaleAfter bounds how long a successful automatic refresh lets no-op
// syncs skip the provider round trip. Provider inventory can change while a
// mailbox is idle, so freshness expires even without mailbox history.
const RefreshStaleAfter = 24 * time.Hour

// NewFastmailInventory constructs the production Fastmail JMAP inventory.
func NewFastmailInventory(apiToken string) Inventory {
	return fastmail.NewClient(apiToken, nil)
}

// Evidence maps provider states to confirmation strength. Historical disabled
// and deleted aliases remain authoritative; pending aliases stay review-only.
func Evidence(records []fastmail.Record) []identityops.ExternalEvidence {
	evidence := make([]identityops.ExternalEvidence, 0, len(records))
	for _, record := range records {
		state := strings.ToLower(strings.TrimSpace(record.State))
		item := identityops.ExternalEvidence{
			Identifier: record.Identifier,
			Signal:     identityops.SignalProviderAlias,
			State:      state,
			Strong:     state == "enabled" || state == "disabled" || state == "deleted",
		}
		if strings.Contains(record.Identifier, "*") {
			item.RejectedReason = "wildcard identity"
		}
		evidence = append(evidence, item)
	}
	return evidence
}

// AutoRefresh applies a configured provider inventory only when the source has
// explicitly opted in. The bool reports whether provider reads were enabled.
func AutoRefresh(
	ctx context.Context,
	cfg *config.Config,
	st Store,
	sourceID int64,
	factory Factory,
) ([]store.IdentityConfirmationOutcome, bool, error) {
	return autoRefresh(ctx, cfg, st, sourceID, factory, false)
}

// AutoRefreshIfDue is AutoRefresh for syncs that observed no mailbox change:
// the provider round trip is skipped while the recorded refresh state is
// fresh, and made when the source has never refreshed, the last attempt
// failed, or the last success is older than RefreshStaleAfter — the cases
// where provider inventory may have moved independently of mailbox history.
func AutoRefreshIfDue(
	ctx context.Context,
	cfg *config.Config,
	st Store,
	sourceID int64,
	factory Factory,
) ([]store.IdentityConfirmationOutcome, bool, error) {
	return autoRefresh(ctx, cfg, st, sourceID, factory, true)
}

func autoRefresh(
	ctx context.Context,
	cfg *config.Config,
	st Store,
	sourceID int64,
	factory Factory,
	skipIfFresh bool,
) ([]store.IdentityConfirmationOutcome, bool, error) {
	if cfg == nil {
		return nil, false, errors.New("provider identity refresh requires configuration")
	}
	configured, err := cfg.FastmailSourceFor(st, sourceID)
	if err != nil {
		return nil, false, fmt.Errorf("resolve Fastmail identity refresh configuration: %w", err)
	}
	if configured == nil || !configured.AutoConfirmIdentities {
		return []store.IdentityConfirmationOutcome{}, false, nil
	}
	if skipIfFresh {
		state, found, stateErr := st.ProviderIdentityRefreshStateContext(ctx, sourceID)
		if stateErr != nil {
			return nil, true, fmt.Errorf("read provider identity refresh state: %w", stateErr)
		}
		if found && state.Fresh(time.Now(), RefreshStaleAfter) {
			return []store.IdentityConfirmationOutcome{}, true, nil
		}
	}
	if factory == nil {
		factory = NewFastmailInventory
	}
	inventory := factory(configured.APIToken)
	if inventory == nil {
		return nil, true, errors.New("fastmail identity inventory unavailable")
	}
	records, err := inventory.ListIdentityRecords(ctx)
	if err == nil {
		var outcomes []store.IdentityConfirmationOutcome
		outcomes, err = identityops.ApplyExternalEvidence(ctx, st, sourceID, Evidence(records))
		if err == nil {
			return outcomes, true, recordRefreshOutcome(ctx, st, sourceID, nil)
		}
	}
	if recordErr := recordRefreshOutcome(ctx, st, sourceID, err); recordErr != nil {
		err = errors.Join(err, recordErr)
	}
	return nil, true, err
}

// recordRefreshOutcome persists the attempt result so no-op syncs know whether
// a retry is owed. A recording failure surfaces to the caller: it means the
// next no-op sync will re-poll the provider, which the operator should see.
func recordRefreshOutcome(ctx context.Context, st Store, sourceID int64, refreshErr error) error {
	if err := st.RecordProviderIdentityRefreshOutcomeContext(ctx, sourceID, refreshErr); err != nil {
		return fmt.Errorf("record provider identity refresh outcome: %w", err)
	}
	return nil
}
