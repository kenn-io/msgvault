// Package provideridentity maps external provider inventory into the shared
// identity evidence contract without giving provider clients store access.
package provideridentity

import (
	"context"
	"encoding/json/v2"
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

// SnapshotInventory supplies a complete JMAP inventory and opaque state.
// Legacy injected inventories retain the evidence-only contract.
type SnapshotInventory interface {
	ListIdentitySnapshot(ctx context.Context) (fastmail.Snapshot, error)
}

// SnapshotStore atomically persists metadata and confirmed ownership.
type SnapshotStore interface {
	ApplyProviderIdentitySnapshotContext(ctx context.Context, sourceID int64, provider, state string, records []store.ProviderIdentityRecord, confirmations []store.IdentityConfirmation) ([]store.IdentityConfirmationOutcome, bool, error)
}

// Snapshot is the shared provider pipeline input. Evidence is validated by
// identityops; metadata never establishes ownership on its own.
type Snapshot struct {
	Provider string
	State    string
	Records  []store.ProviderIdentityRecord
	Evidence []identityops.ExternalEvidence
}

func ApplySnapshot(ctx context.Context, st SnapshotStore, sourceID int64, snapshot Snapshot) ([]store.IdentityConfirmationOutcome, bool, error) {
	return st.ApplyProviderIdentitySnapshotContext(ctx, sourceID, snapshot.Provider, snapshot.State, snapshot.Records, identityops.ExternalEvidenceConfirmations(snapshot.Evidence))
}

func FastmailSnapshot(snapshot fastmail.Snapshot) Snapshot {
	result := Snapshot{Provider: "fastmail", State: snapshot.State, Evidence: Evidence(snapshot.Records)}
	for _, record := range snapshot.Records {
		// JMAP object IDs are scoped to their data type and account.
		key, _ := json.Marshal([]string{record.Kind, record.AccountID, record.ID})
		result.Records = append(result.Records, store.ProviderIdentityRecord{ID: string(key), Identifier: record.Identifier, Kind: record.Kind, State: record.State, ForDomain: record.ForDomain, Description: record.Description, CreatedAt: record.CreatedAt, LastMessageAt: record.LastMessageAt})
	}
	return result
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
	evidence := make([]identityops.ExternalEvidence, 0, 2*len(records))
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
		if record.Kind == "masked-email" {
			item.Signal = identityops.SignalMaskedEmail
			evidence = append(evidence, item)
		}
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
	checks, _ := st.(interface {
		ProviderIdentityCheckFresh(sourceID int64, staleAfter time.Duration) bool
		ProviderIdentityCheckFailed(sourceID int64) bool
		NoteProviderIdentityCheck(sourceID int64, successful bool)
	})
	if skipIfFresh {
		if checks != nil && checks.ProviderIdentityCheckFresh(sourceID, RefreshStaleAfter) {
			return []store.IdentityConfirmationOutcome{}, true, nil
		}
		if checks == nil || !checks.ProviderIdentityCheckFailed(sourceID) {
			state, found, stateErr := st.ProviderIdentityRefreshStateContext(ctx, sourceID)
			if stateErr != nil {
				return nil, true, fmt.Errorf("read provider identity refresh state: %w", stateErr)
			}
			if found && state.Fresh(time.Now(), RefreshStaleAfter) {
				return []store.IdentityConfirmationOutcome{}, true, nil
			}
		}
	}
	if factory == nil {
		factory = NewFastmailInventory
	}
	token, err := cfg.FastmailAPIToken(*configured)
	if err != nil {
		return nil, true, fmt.Errorf("resolve Fastmail credential: %w", err)
	}
	inventory := factory(token)
	if inventory == nil {
		return nil, true, errors.New("fastmail identity inventory unavailable")
	}
	if reader, ok := inventory.(SnapshotInventory); ok {
		writer, ok := st.(SnapshotStore)
		if !ok {
			return nil, true, errors.New("provider metadata store unavailable")
		}
		snapshot, readErr := reader.ListIdentitySnapshot(ctx)
		if readErr != nil {
			if checks != nil {
				checks.NoteProviderIdentityCheck(sourceID, false)
			}
			return nil, true, errors.Join(readErr, recordRefreshOutcome(ctx, st, sourceID, readErr))
		}
		outcomes, changed, applyErr := ApplySnapshot(ctx, writer, sourceID, FastmailSnapshot(snapshot))
		if applyErr != nil {
			if checks != nil {
				checks.NoteProviderIdentityCheck(sourceID, false)
			}
			return nil, true, errors.Join(applyErr, recordRefreshOutcome(ctx, st, sourceID, applyErr))
		}
		if !changed {
			if checks != nil {
				checks.NoteProviderIdentityCheck(sourceID, true)
			}
			return outcomes, true, nil
		}
		return outcomes, true, recordRefreshOutcome(ctx, st, sourceID, nil)
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
	err := st.RecordProviderIdentityRefreshOutcomeContext(ctx, sourceID, refreshErr)
	if checks, ok := st.(interface {
		NoteProviderIdentityCheck(sourceID int64, successful bool)
	}); ok {
		checks.NoteProviderIdentityCheck(sourceID, err == nil && refreshErr == nil)
	}
	if err != nil {
		return fmt.Errorf("record provider identity refresh outcome: %w", err)
	}
	return nil
}
