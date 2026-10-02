package api

import (
	"context"

	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/store"
)

// Store-only projections cover the whole directory. Each network operation
// resolves its persisted owner before selecting credentials.
type cardDAVManagerOperations struct {
	*carddav.Service

	controller *CardDAVController
}

func (m *cardDAVManagerOperations) Sync(ctx context.Context, options carddav.SyncOptions) (carddav.SyncResult, error) {
	return m.controller.Sync(ctx, CardDAVSyncRequest{Full: options.Full})
}

func (m *cardDAVManagerOperations) ListBooks(ctx context.Context) ([]store.CardDAVAddressBook, error) {
	return m.controller.store.ListCardDAVAddressBooksContext(ctx, store.AllCardDAVAccounts)
}

func (m *cardDAVManagerOperations) SetBookRoles(ctx context.Context, id int64, roles carddav.BookRoles) error {
	return m.controller.store.SetCardDAVBookRolesContext(ctx, id, store.CardDAVBookRoles{
		IsWriteTarget: roles.WriteTarget, IsSubscribed: roles.Subscribed, IsLookupSource: roles.LookupSource})
}

func (m *cardDAVManagerOperations) forBook(ctx context.Context, id int64) (CardDAVOperations, error) {
	owner, err := m.controller.store.GetCardDAVAccountForBookContext(ctx, id)
	if err != nil {
		return nil, err
	}
	if owner == nil {
		return nil, store.ErrCardDAVAddressBookNotFound
	}
	selected, err := m.controller.Select(owner.ConnectionName, false)
	if err != nil {
		return nil, carddav.ErrConnectionUnavailable
	}
	service := selected.Current()
	if service == nil {
		return nil, carddav.ErrConnectionUnavailable
	}
	return service, nil
}

func (m *cardDAVManagerOperations) forPerson(ctx context.Context, id int64) (CardDAVOperations, error) {
	source, err := m.controller.store.GetCardDAVPublicationStateSourceContext(ctx, id)
	if err != nil {
		return nil, err
	}
	bookID := source.AddressBookID
	if bookID == 0 {
		bookID = source.ProspectiveBookID
	}
	if bookID == 0 {
		return nil, store.ErrCardDAVNoWriteTarget
	}
	return m.forBook(ctx, bookID)
}

func (m *cardDAVManagerOperations) PublishPerson(ctx context.Context, id int64) error {
	service, err := m.forPerson(ctx, id)
	if err != nil {
		return err
	}
	return service.PublishPerson(ctx, id)
}

func (m *cardDAVManagerOperations) PreviewPublication(ctx context.Context, id int64) (*carddav.PublicationPreview, error) {
	service, err := m.forPerson(ctx, id)
	if err != nil {
		return nil, err
	}
	return service.PreviewPublication(ctx, id)
}

func (m *cardDAVManagerOperations) PublishReviewedPerson(ctx context.Context, id int64, token string) error {
	service, err := m.forPerson(ctx, id)
	if err != nil {
		return err
	}
	return service.PublishReviewedPerson(ctx, id, token)
}

func (m *cardDAVManagerOperations) UnpublishPerson(ctx context.Context, id int64) error {
	service, err := m.forPerson(ctx, id)
	if err != nil {
		return err
	}
	return service.UnpublishPerson(ctx, id)
}

func (m *cardDAVManagerOperations) ResolveConflict(ctx context.Context, id int64, choice carddav.ResolutionChoice) error {
	conflict, err := m.controller.store.GetCardDAVConflictContext(ctx, id)
	if err != nil {
		return err
	}
	service, err := m.forBook(ctx, conflict.AddressBookID)
	if err != nil {
		return err
	}
	return service.ResolveConflict(ctx, id, choice)
}
