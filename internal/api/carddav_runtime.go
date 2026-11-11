package api

import (
	"context"

	"go.kenn.io/msgvault/internal/carddav"
)

// A write-target transfer on any connection can change an iCloud book's
// roles. Retained non-iCloud runtimes therefore share the save lock too.
type cardDAVRoleOperations struct {
	CardDAVOperations

	controller *CardDAVController
}

func (o *cardDAVRoleOperations) SetBookRoles(ctx context.Context, id int64, roles carddav.BookRoles) error {
	o.controller.saveLock().Lock()
	defer o.controller.saveLock().Unlock()
	return o.CardDAVOperations.SetBookRoles(ctx, id, roles)
}
