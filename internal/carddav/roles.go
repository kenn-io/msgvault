package carddav

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
)

type BookRoles struct {
	WriteTarget  bool
	Subscribed   bool
	LookupSource bool
}

type PublicationState string

const (
	PublicationUnpublished PublicationState = "unpublished"
	PublicationPublished   PublicationState = "published"
	PublicationPending     PublicationState = "pending"
	PublicationConflict    PublicationState = "conflict"
)

type PublicationView struct {
	PersonID         int64
	State            PublicationState
	Desired          bool
	PendingOperation store.CardDAVMutationOperation
	AddressBook      *AddressBookIdentity
	ConflictID       *int64
}

func (s *Service) ListBooks(ctx context.Context) ([]store.CardDAVAddressBook, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("CardDAV service is not configured")
	}
	return s.store.ListCardDAVAddressBooksContext(ctx)
}

func (s *Service) SetBookRoles(ctx context.Context, bookID int64, roles BookRoles) error {
	if s == nil || s.store == nil {
		return errors.New("CardDAV service is not configured")
	}
	return s.store.SetCardDAVBookRolesContext(ctx, bookID, store.CardDAVBookRoles{
		IsWriteTarget:  roles.WriteTarget,
		IsSubscribed:   roles.Subscribed,
		IsLookupSource: roles.LookupSource,
	})
}

func (s *Service) PublicationView(ctx context.Context, personID int64) (*PublicationView, error) {
	if s == nil || s.store == nil || personID <= 0 {
		return nil, errors.New("CardDAV service is not configured")
	}
	source, err := s.store.GetCardDAVPublicationStateSourceContext(ctx, personID)
	if err != nil {
		return nil, err
	}
	return publicationViewFromSource(source), nil
}

func publicationViewFromSource(source *store.CardDAVPublicationStateSource) *PublicationView {
	personID := source.PersonID
	view := &PublicationView{PersonID: personID, State: PublicationUnpublished}
	if !source.HasPublication {
		if source.ProspectiveBookID > 0 {
			book := publicAddressBookIdentity(source.ProspectiveBookID, source.ProspectiveName)
			view.AddressBook = &book
		}
		return view
	}
	view.Desired = source.Desired
	view.PendingOperation = source.PendingOperation
	book := publicAddressBookIdentity(source.AddressBookID, source.AddressBookName)
	view.AddressBook = &book
	switch {
	case source.PendingOperation != "":
		view.State = PublicationPending
	case source.ConflictID > 0:
		view.State = PublicationConflict
		conflictID := source.ConflictID
		view.ConflictID = &conflictID
	case source.Desired:
		view.State = PublicationPublished
	default:
		view.State = PublicationUnpublished
	}
	return view
}

func (s *Service) Publication(ctx context.Context, personID int64) (*store.CardDAVPublication, error) {
	if s == nil || s.store == nil || personID <= 0 {
		return nil, errors.New("CardDAV service is not configured")
	}
	return s.store.GetCardDAVPublicationContext(ctx, personID)
}
