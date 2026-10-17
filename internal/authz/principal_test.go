package authz

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRoleAuthority(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		role                  Role
		read, curate, operate bool
	}{
		{"", false, false, false},
		{"unknown", false, false, false},
		{RoleViewer, true, false, false},
		{RoleMember, true, true, false},
		{RoleAdmin, true, true, true},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			p := Principal{Role: tc.role}
			for _, perm := range []Permission{PermissionSearchRead, PermissionMessageRead, PermissionAttachmentRead, PermissionStatsRead, PermissionCalendarRead, PermissionCalendarEventRead, PermissionPersonRead, PermissionSystemRead, PermissionCLIRead} {
				assert.Equal(t, tc.read, p.Has(perm), "%s", perm)
			}
			for _, perm := range []Permission{PermissionCurationWrite, PermissionDraftCreate, PermissionDraftEdit, PermissionDraftDelete, PermissionCalendarWrite, PermissionCalendarInvite, PermissionPersonEdit, PermissionPersonMerge, PermissionCardDAVWrite} {
				assert.Equal(t, tc.curate, p.Has(perm), "%s", perm)
			}
			for _, perm := range []Permission{PermissionCollectionsWrite, PermissionArchiveExport, PermissionDeletionStage, PermissionSQLRead, PermissionArchiveAdmin, PermissionGrantManage} {
				assert.Equal(t, tc.operate, p.Has(perm), "%s", perm)
			}
		})
	}
}

func TestExplicitAuthorityIsNotARole(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()
	p := Principal{Permissions: PermissionSet{PermissionMessageRead: {}}}
	assert.True(p.Has(PermissionMessageRead))
	assert.False(p.Has(PermissionSearchRead))
	assert.False(p.Has(PermissionArchiveAdmin))
	assert.False((Principal{Role: RoleAdmin}).Has("*"))
	assert.False((Principal{Permissions: PermissionSet{"unknown": {}}}).Has("unknown"))
}

func TestRolePermissionsAreIndependent(t *testing.T) {
	t.Parallel()
	permissions := RolePermissions(RoleViewer)
	permissions[PermissionArchiveAdmin] = struct{}{}
	delete(permissions, PermissionMessageRead)
	assert.False(t, (Principal{Role: RoleViewer}).Has(PermissionArchiveAdmin))
	assert.True(t, (Principal{Role: RoleViewer}).Has(PermissionMessageRead))
}

func TestEmptySourceScopeRestrictsAuthority(t *testing.T) {
	t.Parallel()
	assert.False(t, (Principal{}).Restricted())
	assert.True(t, (Principal{Sources: &SourceScope{}}).Restricted())
	assert.True(t, (Principal{Sources: &SourceScope{IDs: []int64{1}}}).Restricted())
}

func FuzzUnknownPermissionNeverAuthorizes(f *testing.F) {
	for _, seed := range []string{"", "*", "message.read", "message.read.extra", "archive.admin", "MESSAGE.READ", "\x00", "☃"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		// Independent vocabulary: new names must make a deliberate change here
		// before a role may authorize them.
		known := map[string]bool{
			"search.read": true, "message.read": true, "attachment.read": true,
			"stats.read": true, "calendar.read": true, "calendar.event.read": true,
			"person.read": true, "system.read": true, "cli.read": true,
			"curation.write": true, "draft.create": true, "draft.edit": true,
			"draft.delete": true, "calendar.write": true, "calendar.invite": true,
			"person.edit": true, "person.merge": true, "carddav.write": true,
			"collections.write": true, "archive.export": true, "deletion.stage": true,
			"sql.read": true, "archive.admin": true, "grant.manage": true,
		}
		assert.Equal(t, known[value], (Principal{Role: RoleAdmin}).Has(Permission(value)))
	})
}
