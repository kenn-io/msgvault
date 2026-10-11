package sync

import (
	"context"
	"errors"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	"net/http"
)

// The native Gmail labels endpoint is complete for the authenticated account.
// Generic archive readers and IMAP's label-shaped mailbox view are not this
// evidence. Pin before the HTTP request; apply only after it fully succeeds.
//
//nolint:nilnil // Generic archive clients have no native label authority; nil keeps archive-only ingestion quiet.
func (s *Syncer) pinNativeGmailLabelInventory(ctx context.Context, sourceID int64) (*store.NativeMessageScopeInventory, error) {
	if _, native := s.client.(nativeMessageReadPermission); !native {
		return nil, nil
	}
	source, err := s.store.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	if source == nil || source.SourceType != sourceTypeGmail {
		return nil, nil
	}
	resource, err := s.store.GetMCPResource(ctx, "source", sourceID)
	if err != nil {
		return nil, err
	}
	return &store.NativeMessageScopeInventory{SourceID: sourceID, SourceReference: resource.Reference, SourceType: sourceTypeGmail, NativeIdentity: source.Identifier, Kind: "gmail_label", Complete: true}, nil
}

// An explicit native authorization denial cannot leave prior scope receipts
// usable. Other failures, including typed quota/network failures, do not prove
// removal. The pre-request account reference rejects stale failure observers.
func (s *Syncer) nativeGmailLabelInventoryError(ctx context.Context, inventory *store.NativeMessageScopeInventory, cause error) error {
	if inventory == nil {
		return cause
	}
	status, denied := errors.AsType[*gmail.StatusError](cause)
	if !denied || status.StatusCode != http.StatusUnauthorized && status.StatusCode != http.StatusForbidden {
		return cause
	}
	observation := store.NativeSourcePermission{SourceID: inventory.SourceID, SourceReference: inventory.SourceReference, SourceType: inventory.SourceType, NativeIdentity: inventory.NativeIdentity, Known: true}
	return errors.Join(cause, s.store.ObserveNativeSourcePermission(ctx, observation))
}
