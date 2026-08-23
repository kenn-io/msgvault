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

func (s *Service) Publication(ctx context.Context, personID int64) (*store.CardDAVPublication, error) {
	if s == nil || s.store == nil || personID <= 0 {
		return nil, errors.New("CardDAV service is not configured")
	}
	return s.store.GetCardDAVPublicationContext(ctx, personID)
}
