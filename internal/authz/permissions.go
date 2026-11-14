// Package authz defines archive principals and their permission vocabulary.
// Credential validation and resource authorization belong to their callers.
package authz

type Permission string

const (
	PermissionVerificationRead  Permission = "verification.read"
	PermissionSearchRead        Permission = "search.read"
	PermissionMessageRead       Permission = "message.read"
	PermissionAttachmentRead    Permission = "attachment.read"
	PermissionStatsRead         Permission = "stats.read"
	PermissionDraftCreate       Permission = "draft.create"
	PermissionDraftEdit         Permission = "draft.edit"
	PermissionDraftDelete       Permission = "draft.delete"
	PermissionCalendarRead      Permission = "calendar.read"
	PermissionCalendarEventRead Permission = "calendar.event.read"
	PermissionCalendarWrite     Permission = "calendar.write"
	PermissionCalendarInvite    Permission = "calendar.invite"
	PermissionPersonRead        Permission = "person.read"
	PermissionPersonEdit        Permission = "person.edit"
	PermissionPersonMerge       Permission = "person.merge"
	PermissionCardDAVWrite      Permission = "carddav.write"
	PermissionSystemRead        Permission = "system.read"
	PermissionCurationWrite     Permission = "curation.write"
	PermissionCLIRead           Permission = "cli.read"
	PermissionCollectionsWrite  Permission = "collections.write"
	PermissionArchiveExport     Permission = "archive.export"
	PermissionDeletionStage     Permission = "deletion.stage"
	PermissionSQLRead           Permission = "sql.read"
	PermissionArchiveAdmin      Permission = "archive.admin"
	PermissionGrantManage       Permission = "grant.manage"
)

// Known capabilities are separate from the subset that grants may carry.
var permissionDefinitions = map[Permission]bool{
	PermissionVerificationRead:  true,
	PermissionSearchRead:        true,
	PermissionMessageRead:       true,
	PermissionAttachmentRead:    true,
	PermissionStatsRead:         true,
	PermissionCalendarRead:      true,
	PermissionCalendarEventRead: true,
	PermissionPersonRead:        false,
	PermissionSystemRead:        false,
	PermissionCLIRead:           false,
	PermissionCurationWrite:     false,
	PermissionDraftCreate:       true,
	PermissionDraftEdit:         true,
	PermissionDraftDelete:       true,
	PermissionCalendarWrite:     true,
	PermissionCalendarInvite:    true,
	PermissionPersonEdit:        false,
	PermissionPersonMerge:       false,
	PermissionCardDAVWrite:      false,
	PermissionCollectionsWrite:  false,
	PermissionArchiveExport:     false,
	PermissionDeletionStage:     false,
	PermissionSQLRead:           false,
	PermissionArchiveAdmin:      false,
	PermissionGrantManage:       false,
}

// KnownPermission accepts only exact names in the closed vocabulary.
func KnownPermission(name string) (Permission, bool) {
	permission := Permission(name)
	_, ok := permissionDefinitions[permission]
	if !ok {
		return "", false
	}
	return permission, true
}

// Grantable reports whether the permission may be issued to an agent grant.
func Grantable(permission Permission) bool {
	return permissionDefinitions[permission]
}
