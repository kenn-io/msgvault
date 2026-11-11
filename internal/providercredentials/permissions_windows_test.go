//go:build windows

package providercredentials

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/fileutil"
	"golang.org/x/sys/windows"
)

func TestWindowsPrepareDirectoryUsesSecureMkdirAll(t *testing.T) {
	require := require.New(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "tokens")
	permissions := nativePermissions{}
	require.NoError(permissions.prepareDirectory(dir))
	require.NoError(permissions.verifyDirectory(dir))
}

func TestWindowsProviderCredentialErrorContext(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	access := uint32(windows.READ_CONTROL)

	path := "provider\x00credential"
	_, expectedErr := windows.UTF16PtrFromString(path)
	requirements.Error(expectedErr)
	_, err := openSecurityHandle(path, false, access)
	requirements.Error(err)
	requirements.ErrorIs(err, expectedErr)
	assertions.Contains(err.Error(), "encode provider credential path:")

	missing := filepath.Join(t.TempDir(), "missing.json")
	missing16, encodeErr := windows.UTF16PtrFromString(missing)
	requirements.NoError(encodeErr)
	_, nativeErr := windows.CreateFile(
		missing16,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	requirements.Error(nativeErr)
	_, err = openSecurityHandle(missing, false, access)
	requirements.Error(err)
	requirements.ErrorIs(err, nativeErr)
	assertions.Contains(err.Error(), "open provider credential security handle:")
}

func TestWindowsProviderCredentialACLValidation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	user := currentWindowsProviderCredentialSID(t)

	dir := filepath.Join(t.TempDir(), "tokens")
	empty, err := Read(dir)
	requirements.NoError(err)
	written, err := Put(dir, empty.ETag, VectorEmbeddingsID,
		"https://embeddings.example.test/v1", "stored-secret")
	requirements.NoError(err)
	loaded, err := Read(dir)
	requirements.NoError(err)
	value, state, err := loaded.Resolve(VectorEmbeddingsID,
		"https://embeddings.example.test/v1", "TEXT_KEY", func(string) (string, bool) {
			return "environment-secret", true
		})
	requirements.NoError(err)
	assertions.Equal("stored-secret", value)
	assertions.Equal(State{Configured: true, Source: SourceStored}, state)
	assertions.NotEqual(empty.ETag, written.ETag)

	storePath := filepath.Join(dir, Filename)
	setProviderCredentialTestACL(t, storePath, []windows.EXPLICIT_ACCESS{
		providerCredentialTestAccess(user, windows.GENERIC_READ, windows.TRUSTEE_IS_USER),
	}, true)
	_, err = Read(dir)
	requirements.ErrorIs(err, ErrUnavailable, "the writable store still requires full control")

	everyone := windowsProviderCredentialSID(t, "S-1-1-0")
	setProviderCredentialTestACL(t, storePath, []windows.EXPLICIT_ACCESS{
		providerCredentialTestAccess(user, windows.GENERIC_ALL, windows.TRUSTEE_IS_USER),
		providerCredentialTestAccess(everyone, windows.GENERIC_READ, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
	}, true)
	var unavailable Snapshot
	unavailable, err = Read(dir)
	requirements.ErrorIs(err, ErrUnavailable)
	value, _, err = unavailable.Resolve(VectorEmbeddingsID,
		"https://embeddings.example.test/v1", "TEXT_KEY", func(string) (string, bool) {
			return "must-not-fallback", true
		})
	requirements.ErrorIs(err, ErrUnavailable)
	assertions.Empty(value)

	tests := []struct {
		name      string
		entries   []windows.EXPLICIT_ACCESS
		protected bool
		wantErr   bool
		check     func(*testing.T, *windows.ACL, windows.SECURITY_DESCRIPTOR_CONTROL)
	}{
		{
			name:      "valid mapped file all access",
			entries:   []windows.EXPLICIT_ACCESS{providerCredentialTestAccess(user, windows.GENERIC_ALL, windows.TRUSTEE_IS_USER)},
			protected: true,
			wantErr:   false,
			check: func(t *testing.T, dacl *windows.ACL, control windows.SECURITY_DESCRIPTOR_CONTROL) {
				t.Helper()
				require := require.New(t)
				require.NotNil(dacl)
				require.Equal(uint16(1), dacl.AceCount)
				var ace *windows.ACCESS_ALLOWED_ACE
				require.NoError(windows.GetAce(dacl, 0, &ace))
				require.Equal(uint32(fileutil.FileAllAccess), uint32(ace.Mask))
				require.NotZero(control & windows.SE_DACL_PROTECTED)
			},
		},
		{
			name:      "zero ACEs",
			entries:   nil,
			protected: true,
			wantErr:   true,
			check: func(t *testing.T, dacl *windows.ACL, _ windows.SECURITY_DESCRIPTOR_CONTROL) {
				t.Helper()
				assert := assert.New(t)
				assert.True(dacl == nil || dacl.AceCount == 0)
			},
		},
		{
			name: "additional principal",
			entries: []windows.EXPLICIT_ACCESS{
				providerCredentialTestAccess(user, windows.GENERIC_ALL, windows.TRUSTEE_IS_USER),
				providerCredentialTestAccess(everyone, windows.GENERIC_READ, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
			},
			protected: true,
			check: func(t *testing.T, dacl *windows.ACL, _ windows.SECURITY_DESCRIPTOR_CONTROL) {
				t.Helper()
				require := require.New(t)
				require.NotNil(dacl)
				require.GreaterOrEqual(int(dacl.AceCount), 2)
			},
			wantErr: true,
		},
		{
			name:      "inherited or unprotected",
			entries:   []windows.EXPLICIT_ACCESS{providerCredentialTestAccess(user, windows.GENERIC_ALL, windows.TRUSTEE_IS_USER)},
			protected: false,
			wantErr:   true,
			check: func(t *testing.T, _ *windows.ACL, control windows.SECURITY_DESCRIPTOR_CONTROL) {
				t.Helper()
				assert.Zero(t, control&windows.SE_DACL_PROTECTED)
			},
		},
		{
			name:      "wrong SID",
			entries:   []windows.EXPLICIT_ACCESS{providerCredentialTestAccess(everyone, windows.GENERIC_ALL, windows.TRUSTEE_IS_WELL_KNOWN_GROUP)},
			protected: true,
			wantErr:   true,
			check: func(t *testing.T, dacl *windows.ACL, _ windows.SECURITY_DESCRIPTOR_CONTROL) {
				t.Helper()
				require := require.New(t)
				require.NotNil(dacl)
				require.Equal(uint16(1), dacl.AceCount)
			},
		},
		{
			name:      "insufficient access mask",
			entries:   []windows.EXPLICIT_ACCESS{providerCredentialTestAccess(user, windows.GENERIC_READ, windows.TRUSTEE_IS_USER)},
			protected: true,
			wantErr:   true,
			check: func(t *testing.T, dacl *windows.ACL, _ windows.SECURITY_DESCRIPTOR_CONTROL) {
				t.Helper()
				require := require.New(t)
				require.NotNil(dacl)
				require.Equal(uint16(1), dacl.AceCount)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeProviderCredentialTestFile(t)
			handle := setProviderCredentialTestACL(t, path, tt.entries, tt.protected)
			dacl, control := readProviderCredentialTestACL(t, handle)
			tt.check(t, dacl, control)
			if tt.wantErr {
				require.Error(t, verifyOwnerOnlyHandleForUser(handle, user, false))
			} else {
				require.NoError(t, verifyOwnerOnlyHandleForUser(handle, user, false))
			}
		})
	}
}

func TestWindowsReadSecretFileACL(t *testing.T) {
	t.Parallel()
	user := currentWindowsProviderCredentialSID(t)
	everyone := windowsProviderCredentialSID(t, "S-1-1-0")
	for _, tc := range []struct {
		name      string
		entries   []windows.EXPLICIT_ACCESS
		protected bool
		wantError bool
	}{
		{
			name:      "owner read only",
			entries:   []windows.EXPLICIT_ACCESS{providerCredentialTestAccess(user, windows.GENERIC_READ, windows.TRUSTEE_IS_USER)},
			protected: true,
		},
		{
			name: "additional principal",
			entries: []windows.EXPLICIT_ACCESS{
				providerCredentialTestAccess(user, windows.GENERIC_READ, windows.TRUSTEE_IS_USER),
				providerCredentialTestAccess(everyone, windows.GENERIC_READ, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
			},
			protected: true,
			wantError: true,
		},
		{
			name:      "unprotected",
			entries:   []windows.EXPLICIT_ACCESS{providerCredentialTestAccess(user, windows.GENERIC_READ, windows.TRUSTEE_IS_USER)},
			wantError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			path := writeProviderCredentialTestFile(t)
			handle := setProviderCredentialTestACL(t, path, tc.entries, tc.protected)
			value, err := ReadSecretFile(path)
			if tc.wantError {
				require.Error(err)
				assert.Empty(value)
				return
			}
			require.NoError(err)
			assert.Equal("synthetic-secret", value)
			// Reading a mounted secret must not change its read-only ACL.
			dacl, _ := readProviderCredentialTestACL(t, handle)
			var ace *windows.ACCESS_ALLOWED_ACE
			require.NoError(windows.GetAce(dacl, 0, &ace))
			assert.Equal(uint32(windows.FILE_GENERIC_READ), uint32(ace.Mask))
		})
	}
	t.Run("restore Events credential and read-only secret", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		dir := filepath.Join(t.TempDir(), "tokens")
		id := EventsID("0123456789abcdef0123456789abcdef")
		empty, err := ReadEvents(dir)
		requirements.NoError(err)
		revision, err := empty.Revision(id)
		requirements.NoError(err)
		_, err = PutIfRevision(dir, revision, id, "https://example.test", "synthetic-secret")
		requirements.NoError(err)
		requirements.NoError(os.Remove(filepath.Join(dir, ".provider-credentials.lock")))
		eventsPath := filepath.Join(dir, EventsFilename)
		secretPath := filepath.Join(dir, "ordinary-secret")
		requirements.NoError(os.WriteFile(secretPath, []byte("ordinary-secret"), 0600))
		access := []windows.EXPLICIT_ACCESS{providerCredentialTestAccess(user, windows.GENERIC_READ, windows.TRUSTEE_IS_USER)}
		setProviderCredentialTestACL(t, eventsPath, access, true)
		secretHandle := setProviderCredentialTestACL(t, secretPath, access, true)
		_, err = ReadEvents(dir)
		requirements.Error(err)
		requirements.NoError(SecureRestored(dir, []string{EventsFilename, "ordinary-secret"}))
		restored, err := ReadEvents(dir)
		requirements.NoError(err)
		value, _, err := restored.Resolve(id, "https://example.test", "", nil)
		requirements.NoError(err)
		assertions.Equal("synthetic-secret", value)
		secret, err := ReadSecretFile(secretPath)
		requirements.NoError(err)
		assertions.Equal("ordinary-secret", secret)
		dacl, _ := readProviderCredentialTestACL(t, secretHandle)
		var ace *windows.ACCESS_ALLOWED_ACE
		requirements.NoError(windows.GetAce(dacl, 0, &ace))
		assertions.Equal(uint32(windows.FILE_GENERIC_READ), uint32(ace.Mask))
	})
}

func TestWindowsCredentialWritesRepairReadOnlyDirectory(t *testing.T) {
	for _, operation := range []string{"server key", "provider credential"} {
		t.Run(operation, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			dir := filepath.Join(t.TempDir(), "tokens")
			require.NoError(os.Mkdir(dir, 0o700))
			handle := setProviderCredentialTestACL(t, dir, []windows.EXPLICIT_ACCESS{
				providerCredentialTestAccess(currentWindowsProviderCredentialSID(t), windows.GENERIC_READ, windows.TRUSTEE_IS_USER),
			}, true)
			// Restore write access even if the operation fails so TempDir can clean up.
			t.Cleanup(func() { assert.NoError(secureOwnerOnlyHandle(handle)) })
			entries, err := os.ReadDir(dir)
			require.NoError(err)
			require.Empty(entries, "repair must work before a lock file exists")
			file, err := os.OpenFile(filepath.Join(dir, ".provider-credentials.lock"), os.O_RDWR|os.O_CREATE, 0o600)
			if file != nil {
				require.NoError(file.Close())
			}
			require.ErrorIs(err, os.ErrPermission, "the fixture must deny lock-file creation")

			if operation == "server key" {
				key, err := EnsureServerKey(dir)
				require.NoError(err)
				require.NotEmpty(key)
				persisted, err := ReadSecretFile(filepath.Join(dir, ServerKeyFilename))
				require.NoError(err)
				assert.Equal(key, persisted)
				return
			}
			empty, err := Read(dir)
			require.NoError(err)
			_, err = Put(dir, empty.ETag, VectorEmbeddingsID, "https://embeddings.example.test/v1", "stored-secret")
			require.NoError(err)
			stored, err := Read(dir)
			require.NoError(err)
			value, state, err := stored.Resolve(VectorEmbeddingsID, "https://embeddings.example.test/v1", "", nil)
			require.NoError(err)
			assert.Equal("stored-secret", value)
			assert.Equal(State{Configured: true, Source: SourceStored}, state)
		})
	}
}

func TestWindowsProviderCredentialOwnerValidation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	user := currentWindowsProviderCredentialSID(t)

	requirements.NoError(verifyWindowsOwner(user, user))
	foreign := windowsProviderCredentialSID(t, "S-1-1-0")
	err := verifyWindowsOwner(foreign, user)
	requirements.Error(err)
	assertions.Contains(err.Error(), "provider credential owner is not the current user")

	administrators := windowsProviderCredentialSID(t, "S-1-5-32-544")
	member, err := windows.Token(0).IsMember(administrators)
	requirements.NoError(err)
	err = verifyWindowsOwner(administrators, user)
	if member {
		requirements.NoError(err)
	} else {
		requirements.Error(err)
	}
}

func TestWindowsProviderCredentialReparseRejection(t *testing.T) {
	requirements := require.New(t)
	root := t.TempDir()
	target := filepath.Join(root, "target")
	junction := filepath.Join(root, "junction")
	requirements.NoError(os.Mkdir(target, 0o700))
	output, err := exec.CommandContext(t.Context(), "cmd.exe", "/d", "/c", "mklink", "/J", junction, target).CombinedOutput()
	if err != nil {
		t.Skipf("create directory junction: %v: %s", err, output)
	}
	t.Cleanup(func() {
		requirements.NoError(os.Remove(junction))
		requirements.NoError(os.RemoveAll(target))
	})

	_, err = openSecurityHandle(junction, true, windows.READ_CONTROL)
	requirements.Error(err)
	requirements.Contains(err.Error(), "reparse point")
	_, err = EnsureServerKey(junction)
	requirements.ErrorContains(err, "reparse point")
	_, err = Put(junction, "", VectorEmbeddingsID, "https://embeddings.example.test/v1", "synthetic-secret")
	requirements.ErrorContains(err, "reparse point")
	entries, err := os.ReadDir(target)
	requirements.NoError(err)
	assert.Empty(t, entries, "reject the directory before creating a lock file")
}

func currentWindowsProviderCredentialSID(t *testing.T) *windows.SID {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)
	return user.User.Sid
}

func windowsProviderCredentialSID(t *testing.T, values ...string) *windows.SID {
	t.Helper()
	sid := "S-1-1-0"
	if len(values) > 0 {
		sid = values[0]
	}
	value, err := windows.StringToSid(sid)
	require.NoError(t, err)
	return value
}

func providerCredentialTestAccess(sid *windows.SID, permissions windows.ACCESS_MASK, trusteeType windows.TRUSTEE_TYPE) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: permissions,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  trusteeType,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}

func writeProviderCredentialTestFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential.json")
	require.NoError(t, os.WriteFile(path, []byte("synthetic-secret"), 0o600))
	return path
}

func setProviderCredentialTestACL(t *testing.T, path string, entries []windows.EXPLICIT_ACCESS, protected bool) windows.Handle {
	t.Helper()
	requirements := require.New(t)
	info, err := os.Stat(path)
	requirements.NoError(err)
	handle, err := openSecurityHandle(path, info.IsDir(), windows.READ_CONTROL|windows.WRITE_DAC)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(windows.CloseHandle(handle)) })
	securityInfo := windows.DACL_SECURITY_INFORMATION
	if protected {
		securityInfo |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	if len(entries) == 0 {
		descriptor, descriptorErr := windows.SecurityDescriptorFromString("D:P")
		requirements.NoError(descriptorErr)
		requirements.NoError(windows.SetKernelObjectSecurity(handle,
			windows.SECURITY_INFORMATION(securityInfo), descriptor))
		return handle
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	requirements.NoError(err)
	requirements.NoError(windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.SECURITY_INFORMATION(securityInfo), nil, nil, acl, nil))
	return handle
}

func readProviderCredentialTestACL(t *testing.T, handle windows.Handle) (*windows.ACL, windows.SECURITY_DESCRIPTOR_CONTROL) {
	t.Helper()
	requirements := require.New(t)
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	requirements.NoError(err)
	control, _, err := descriptor.Control()
	requirements.NoError(err)
	dacl, _, err := descriptor.DACL()
	requirements.NoError(err)
	return dacl, control
}

func TestRestorePreflightAllowsOmittedFileWithoutInheritedAccess(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	dir := filepath.Join(t.TempDir(), "tokens")
	requirements.NoError((nativePermissions{}).secureDirectory(dir))
	path := filepath.Join(dir, "omitted")
	requirements.NoError(os.WriteFile(path, []byte("fixture"), 0600))
	handle, err := openSecurityHandle(path, false, windows.READ_CONTROL)
	requirements.NoError(err)
	defer windows.CloseHandle(handle) //nolint:errcheck // test handle
	before, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	requirements.NoError(err)
	user := currentWindowsProviderCredentialSID(t)
	setProviderCredentialTestACL(t, dir, []windows.EXPLICIT_ACCESS{
		providerCredentialTestAccess(user, windows.GENERIC_ALL, windows.TRUSTEE_IS_USER),
		providerCredentialTestAccess(windowsProviderCredentialSID(t, "S-1-1-0"), windows.GENERIC_READ, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
	}, true)
	selected := filepath.Join(dir, "selected")
	requirements.NoError(os.WriteFile(selected, []byte("selected"), 0600))
	requirements.NoError(SecureRestored(dir, []string{"selected"}))
	after, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	requirements.NoError(err)
	assertions.Equal(before.String(), after.String())
}

func TestSecureRestoredRefusesInheritedOmittedACLBeforeChanges(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	dir := filepath.Join(t.TempDir(), "tokens")
	requirements.NoError(os.Mkdir(dir, 0700))
	user := currentWindowsProviderCredentialSID(t)
	access := providerCredentialTestAccess(user, windows.GENERIC_ALL, windows.TRUSTEE_IS_USER)
	access.Inheritance = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	setProviderCredentialTestACL(t, dir, []windows.EXPLICIT_ACCESS{access, providerCredentialTestAccess(windowsProviderCredentialSID(t, "S-1-1-0"), windows.GENERIC_READ, windows.TRUSTEE_IS_WELL_KNOWN_GROUP)}, true)
	selected := filepath.Join(dir, "selected")
	omitted := filepath.Join(dir, "omitted")
	requirements.NoError(os.WriteFile(selected, []byte("selected"), 0600))
	requirements.NoError(os.WriteFile(omitted, []byte("omitted"), 0600))
	descriptor := func(path string, directory bool) string {
		t.Helper()
		h, err := openSecurityHandle(path, directory, windows.READ_CONTROL)
		requirements.NoError(err)
		defer windows.CloseHandle(h) //nolint:errcheck
		sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		requirements.NoError(err)
		return sd.String()
	}
	beforeDir, beforeSelected, beforeOmitted := descriptor(dir, true), descriptor(selected, false), descriptor(omitted, false)
	err := SecureRestored(dir, []string{"selected"})
	requirements.ErrorIs(err, ErrRestorePermissionsConflict)
	assertions.Equal(beforeDir, descriptor(dir, true))
	assertions.Equal(beforeSelected, descriptor(selected, false))
	assertions.Equal(beforeOmitted, descriptor(omitted, false))
	_, err = os.Stat(filepath.Join(dir, ".provider-credentials.lock"))
	assertions.ErrorIs(err, os.ErrNotExist)
}

func TestRestorePreflightRejectsReparseEntry(t *testing.T) {
	requirements := require.New(t)
	dir := filepath.Join(t.TempDir(), "tokens")
	requirements.NoError((nativePermissions{}).secureDirectory(dir))
	target := filepath.Join(t.TempDir(), "target")
	requirements.NoError(os.WriteFile(target, []byte("fixture"), 0600))
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		t.Skipf("creating a symlink requires Windows privileges: %v", err)
	}
	_, err := restorePreflight(dir, map[string]struct{}{}, map[string]struct{}{}, true)
	requirements.ErrorIs(err, ErrRestorePermissionsConflict)
	selected := filepath.Join(dir, "selected")
	requirements.NoError(os.WriteFile(selected, []byte("selected"), 0o600))
	requirements.NoError(SecureRestored(dir, []string{"selected"}))
}

func TestSecureRestoredPreservesProtectedMixedTree(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	dir := filepath.Join(t.TempDir(), "tokens")
	requirements.NoError((nativePermissions{}).secureDirectory(dir))
	user := currentWindowsProviderCredentialSID(t)
	nested := filepath.Join(dir, "nested")
	requirements.NoError(os.Mkdir(nested, 0700))
	access := providerCredentialTestAccess(user, windows.GENERIC_ALL, windows.TRUSTEE_IS_USER)
	access.Inheritance = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	setProviderCredentialTestACL(t, nested, []windows.EXPLICIT_ACCESS{access}, true)
	setProviderCredentialTestACL(t, dir, []windows.EXPLICIT_ACCESS{
		providerCredentialTestAccess(user, windows.GENERIC_ALL, windows.TRUSTEE_IS_USER),
		providerCredentialTestAccess(windowsProviderCredentialSID(t, "S-1-1-0"), windows.GENERIC_READ, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
	}, true)
	changedAncestor := filepath.Join(nested, "changed")
	requirements.NoError(os.Mkdir(changedAncestor, 0700))
	selectedBelow := filepath.Join(changedAncestor, "selected")
	omittedBelow := filepath.Join(changedAncestor, "omitted")
	requirements.NoError(os.WriteFile(selectedBelow, []byte("selected"), 0600))
	requirements.NoError(os.WriteFile(omittedBelow, []byte("omitted"), 0600))
	handle, err := openSecurityHandle(omittedBelow, false, windows.READ_CONTROL)
	requirements.NoError(err)
	defer windows.CloseHandle(handle) //nolint:errcheck // test handle
	before, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	requirements.NoError(err)
	requirements.NoError(SecureRestored(dir, []string{filepath.Join("nested", "changed", "selected")}))
	after, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	requirements.NoError(err)
	assertions.Equal(before.String(), after.String())
}

func TestWindowsDirectorySecurityRemovesInheritedChildAccess(t *testing.T) {
	requirements := require.New(t)
	dir := filepath.Join(t.TempDir(), "tokens")
	requirements.NoError(os.Mkdir(dir, 0700))
	user := currentWindowsProviderCredentialSID(t)
	access := providerCredentialTestAccess(user, windows.GENERIC_ALL, windows.TRUSTEE_IS_USER)
	access.Inheritance = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	setProviderCredentialTestACL(t, dir, []windows.EXPLICIT_ACCESS{access}, true)
	path := filepath.Join(dir, "child")
	requirements.NoError(os.WriteFile(path, []byte("fixture"), 0600))
	handle, err := openSecurityHandle(path, false, windows.READ_CONTROL)
	requirements.NoError(err)
	defer windows.CloseHandle(handle) //nolint:errcheck // test handle
	inherited, err := inheritedAccess(handle)
	requirements.NoError(err)
	requirements.True(inherited)
	requirements.NoError((nativePermissions{}).secureDirectory(dir))
	inherited, err = inheritedAccess(handle)
	requirements.NoError(err)
	requirements.False(inherited)
}

func TestSecureRestoredDoesNotTouchEmptySelection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tokens")
	require.NoError(t, os.Mkdir(dir, 0700))
	user := currentWindowsProviderCredentialSID(t)
	access := providerCredentialTestAccess(user, windows.GENERIC_ALL, windows.TRUSTEE_IS_USER)
	access.Inheritance = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	root := setProviderCredentialTestACL(t, dir, []windows.EXPLICIT_ACCESS{access, providerCredentialTestAccess(windowsProviderCredentialSID(t, "S-1-1-0"), windows.GENERIC_READ, windows.TRUSTEE_IS_WELL_KNOWN_GROUP)}, true)
	path := filepath.Join(dir, "omitted")
	require.NoError(t, os.WriteFile(path, []byte("fixture"), 0600))
	child, err := openSecurityHandle(path, false, windows.READ_CONTROL)
	require.NoError(t, err)
	defer windows.CloseHandle(child) //nolint:errcheck // test handle
	beforeRoot, err := windows.GetSecurityInfo(root, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	beforeChild, err := windows.GetSecurityInfo(child, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	require.NoError(t, SecureRestored(dir, nil))
	afterRoot, err := windows.GetSecurityInfo(root, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	afterChild, err := windows.GetSecurityInfo(child, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	assert.Equal(t, beforeRoot.String(), afterRoot.String())
	assert.Equal(t, beforeChild.String(), afterChild.String())
}

func TestSecureRestoredCaseVariantProviderRequiresFullAccess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tokens")
	require.NoError(t, (nativePermissions{}).secureDirectory(dir))
	name := "Provider-Credentials.json"
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("fixture"), 0o600))
	user := currentWindowsProviderCredentialSID(t)
	handle := setProviderCredentialTestACL(t, path, []windows.EXPLICIT_ACCESS{providerCredentialTestAccess(user, windows.GENERIC_READ, windows.TRUSTEE_IS_USER)}, true)
	require.NoError(t, SecureRestored(dir, []string{name}))
	require.NoError(t, verifyOwnerOnlyHandle(handle, false))
}
