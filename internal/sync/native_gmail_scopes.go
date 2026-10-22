package sync

import (
	"context"
	"errors"
	"go.kenn.io/msgvault/internal/store"
)

var errNativeGmailLabelUnavailable = errors.New("native Gmail label unavailable")

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
