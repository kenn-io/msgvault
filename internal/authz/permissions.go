// Package authz defines archive principals and their permission vocabulary.
// Credential validation and resource authorization belong to their callers.
package authz

type Permission string

const (
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

type Role string

const (
	RoleViewer Role = "viewer"
	RoleMember Role = "member"
	RoleAdmin  Role = "admin"
)

type permissionDefinition struct {
	minimumRole Role
	grantable   bool
}

// Keep role membership and grantability separate: administrative capabilities
// must not become valid grant permissions merely because their name is known.
var permissionDefinitions = map[Permission]permissionDefinition{
	PermissionSearchRead:        {RoleViewer, true},
	PermissionMessageRead:       {RoleViewer, true},
	PermissionAttachmentRead:    {RoleViewer, true},
	PermissionStatsRead:         {RoleViewer, true},
	PermissionCalendarRead:      {RoleViewer, true},
	PermissionCalendarEventRead: {RoleViewer, true},
	PermissionPersonRead:        {RoleViewer, true},
	PermissionSystemRead:        {RoleViewer, false},
	PermissionCLIRead:           {RoleViewer, false},
	PermissionCurationWrite:     {RoleMember, false},
	PermissionDraftCreate:       {RoleMember, true},
	PermissionDraftEdit:         {RoleMember, true},
	PermissionDraftDelete:       {RoleMember, true},
	PermissionCalendarWrite:     {RoleMember, true},
	PermissionCalendarInvite:    {RoleMember, true},
	PermissionPersonEdit:        {RoleMember, true},
	PermissionPersonMerge:       {RoleMember, true},
	PermissionCardDAVWrite:      {RoleMember, true},
	PermissionCollectionsWrite:  {RoleAdmin, false},
	PermissionArchiveExport:     {RoleAdmin, false},
	PermissionDeletionStage:     {RoleAdmin, false},
	PermissionSQLRead:           {RoleAdmin, false},
	PermissionArchiveAdmin:      {RoleAdmin, false},
	PermissionGrantManage:       {RoleAdmin, false},
}

type PermissionSet map[Permission]struct{}

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
	return permissionDefinitions[permission].grantable
}

// RolePermissions returns an independent set; mutations cannot change roles.
func RolePermissions(role Role) PermissionSet {
	permissions := make(PermissionSet)
	for permission, definition := range permissionDefinitions {
		if roleRank(role) >= roleRank(definition.minimumRole) {
			permissions[permission] = struct{}{}
		}
	}
	return permissions
}

func roleRank(role Role) int {
	switch role {
	case RoleViewer:
		return 1
	case RoleMember:
		return 2
	case RoleAdmin:
		return 3
	default:
		return 0
	}
}
