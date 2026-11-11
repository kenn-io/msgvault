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

func FuzzPermissionVocabulary(f *testing.F) {
	grantable := map[string]bool{
		"verification.read": true, "search.read": true, "message.read": true,
		"attachment.read": true, "stats.read": true, "draft.create": true,
		"draft.edit": true, "draft.delete": true, "calendar.read": true,
		"calendar.event.read": true, "calendar.write": true, "calendar.invite": true,
	}
	roleOnly := map[string]bool{
		"person.read": true, "person.edit": true, "person.merge": true, "carddav.write": true,
		"system.read": true, "curation.write": true, "cli.read": true, "collections.write": true,
		"archive.export": true, "deletion.stage": true, "sql.read": true, "archive.admin": true, "grant.manage": true,
	}
	for name := range grantable {
		f.Add(name)
	}
	for name := range roleOnly {
		f.Add(name)
	}
	for _, seed := range []string{"", "*", "message.read.extra", "MESSAGE.READ", "message.read\x00", " message.read", "message.reаd", "☃"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		wantKnown := grantable[value] || roleOnly[value]
		permission, known := KnownPermission(value)
		assert.Equal(t, wantKnown, known)
		if wantKnown {
			assert.Equal(t, Permission(value), permission)
		} else {
			assert.Empty(t, permission)
		}
		assert.Equal(t, grantable[value], Grantable(Permission(value)))
		assert.Equal(t, wantKnown, (Principal{Role: RoleAdmin}).Has(Permission(value)))
	})
}
