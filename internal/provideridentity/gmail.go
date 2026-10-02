package provideridentity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/store"
)

// GmailSnapshot uses Gmail's primary/accepted send-as entries as strong
// ownership evidence. Pending entries remain available for review. Callers
// require an explicit owner apply or configured opt-in before ApplySnapshot.
func GmailSnapshot(entries []gmail.SendAs) Snapshot {
	snapshot := Snapshot{Provider: "gmail-send-as"}
	for _, entry := range entries {
		identifier := strings.TrimSpace(entry.Email)
		state := strings.ToLower(strings.TrimSpace(entry.VerificationStatus))
		if entry.Primary {
			state = "primary"
		}
		item := identityops.ExternalEvidence{Identifier: identifier, State: state, Signal: identityops.SignalProviderAlias, Strong: entry.Primary || state == "accepted"}
		if strings.Contains(identifier, "*") {
			item.RejectedReason = "wildcard identity"
		}
		snapshot.Evidence = append(snapshot.Evidence, item)
		snapshot.Records = append(snapshot.Records, store.ProviderIdentityRecord{ID: store.NormalizeIdentifierForCompare(identifier), Identifier: identifier, Kind: "send-as", State: state, Description: entry.DisplayName})
	}
	sort.Slice(snapshot.Records, func(i, j int) bool { return snapshot.Records[i].ID < snapshot.Records[j].ID })
	digest := sha256.New()
	for _, record := range snapshot.Records {
		// Length-prefixed raw values give a stable state without JSON tag or
		// UTF-8 coercion differences. Inventory ordering is irrelevant.
		for _, value := range []string{record.ID, record.Identifier, record.State, record.Description} {
			_, _ = fmt.Fprintf(digest, "%d:%s", len(value), value)
		}
	}
	snapshot.State = hex.EncodeToString(digest.Sum(nil))
	return snapshot
}

// GmailInventory reads provider ownership metadata, without sending mail.
type GmailInventory interface {
	ListSendAs(ctx context.Context) ([]gmail.SendAs, error)
}

// ApplyGmailSendAs applies all current strong entries after explicit owner
// consent or configured opt-in. Selective bootstrap uses ApplyGmailSendAsSelection.
// A failed provider read never mutates metadata or ownership.
func ApplyGmailSendAs(ctx context.Context, st SnapshotStore, sourceID int64, inventory GmailInventory) ([]store.IdentityConfirmationOutcome, bool, error) {
	entries, err := inventory.ListSendAs(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("read Gmail send-as identities: %w", err)
	}
	return ApplySnapshot(ctx, st, sourceID, GmailSnapshot(entries))
}

// OwnerSnapshotStore atomically persists metadata with explicit confirmations
// independently of whether the provider inventory has changed.
type OwnerSnapshotStore interface {
	ConfirmProviderIdentitySnapshotContext(ctx context.Context, sourceID int64, provider, state string, records []store.ProviderIdentityRecord, confirmations []store.IdentityConfirmation) ([]store.IdentityConfirmationOutcome, bool, error)
}

// ApplyGmailSendAsSelection saves the complete inventory and confirms only the
// owner's selected primary/accepted mailboxes. Read-only previews must not call
// it. Invalid selections fail before any metadata or ownership writes. The bool
// reports inventory changes; outcomes report selected ownership merges.
func ApplyGmailSendAsSelection(ctx context.Context, st OwnerSnapshotStore, sourceID int64, entries []gmail.SendAs, selected []string) ([]store.IdentityConfirmationOutcome, bool, error) {
	if len(selected) == 0 {
		return nil, false, errors.New("gmail send-as confirmation requires a selection")
	}
	snapshot := GmailSnapshot(entries)
	strong := make(map[string]store.IdentityConfirmation)
	for _, confirmation := range identityops.ExternalEvidenceConfirmations(snapshot.Evidence) {
		confirmation.Signals = append(confirmation.Signals, "gmail-send-as")
		strong[store.NormalizeIdentifierForCompare(confirmation.Identifier)] = confirmation
	}
	confirmations := make([]store.IdentityConfirmation, 0, len(selected))
	seen := make(map[string]bool)
	for _, address := range selected {
		key := store.NormalizeIdentifierForCompare(strings.TrimSpace(address))
		confirmation, ok := strong[key]
		if !ok {
			return nil, false, errors.New("selected Gmail send-as identity is not a current primary or accepted mailbox")
		}
		if !seen[key] {
			seen[key] = true
			confirmations = append(confirmations, confirmation)
		}
	}
	return st.ConfirmProviderIdentitySnapshotContext(ctx, sourceID, snapshot.Provider, snapshot.State, snapshot.Records, confirmations)
}
