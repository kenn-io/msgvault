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

func restoredTestDescriptor(t *testing.T, path string, directory bool) string {
	t.Helper()
	handle, err := openSecurityHandle(path, directory, windows.READ_CONTROL)
	require.NoError(t, err)
	defer windows.CloseHandle(handle) //nolint:errcheck // test handle
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	return descriptor.String()
}

// shareRestoredTestDirectory grants Everyone access beside the owner, as an existing token tree might.
func shareRestoredTestDirectory(t *testing.T, dir string, everyone windows.ACCESS_MASK, inheritance uint32) windows.Handle {
	t.Helper()
	owner := providerCredentialTestAccess(currentWindowsProviderCredentialSID(t), windows.GENERIC_ALL, windows.TRUSTEE_IS_USER)
	other := providerCredentialTestAccess(windowsProviderCredentialSID(t), everyone, windows.TRUSTEE_IS_WELL_KNOWN_GROUP)
	owner.Inheritance, other.Inheritance = inheritance, inheritance
	return setProviderCredentialTestACL(t, dir, []windows.EXPLICIT_ACCESS{owner, other}, true)
}

func TestSecureRestoredPreservesOrRejectsInheritedOmittedACL(t *testing.T) {
	t.Run("omitted without inherited access", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "tokens")
		require.NoError(t, (nativePermissions{}).secureDirectory(dir))
		path := filepath.Join(dir, "omitted")
		require.NoError(t, os.WriteFile(path, []byte("fixture"), 0600))
		before := restoredTestDescriptor(t, path, false)
		root := shareRestoredTestDirectory(t, dir, windows.GENERIC_READ, windows.NO_INHERITANCE)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "selected"), []byte("selected"), 0600))
		require.NoError(t, SecureRestored(dir, []string{"selected"}))
		require.NoError(t, verifyOwnerOnlyHandle(root, false))
		assert.Equal(t, before, restoredTestDescriptor(t, path, false))
	})
	t.Run("omitted with inherited access", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		dir := filepath.Join(t.TempDir(), "tokens")
		requirements.NoError(os.Mkdir(dir, 0700))
		shareRestoredTestDirectory(t, dir, windows.GENERIC_READ, windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE)
		selected := filepath.Join(dir, "NESTED", "SELECTED")
		omitted := filepath.Join(dir, "omitted")
		requirements.NoError(os.Mkdir(filepath.Dir(selected), 0700))
		requirements.NoError(os.WriteFile(selected, []byte("selected"), 0600))
		requirements.NoError(os.WriteFile(omitted, []byte("omitted"), 0600))
		beforeDir, beforeOmitted := restoredTestDescriptor(t, dir, true), restoredTestDescriptor(t, omitted, false)
		err := SecureRestored(dir, []string{filepath.Join("nested", "selected")})
		requirements.ErrorIs(err, ErrRestorePermissionsConflict)
		assertions.Equal(beforeDir, restoredTestDescriptor(t, dir, true))
		assertions.Equal(beforeOmitted, restoredTestDescriptor(t, omitted, false))
		h, err := openSecurityHandle(selected, false, windows.READ_CONTROL)
		requirements.NoError(err)
		defer windows.CloseHandle(h) //nolint:errcheck
		requirements.NoError(verifyOwnerOnlyHandle(h, false))
		requirements.NoError(os.Remove(omitted))
		requirements.NoError(SecureRestored(dir, []string{filepath.Join("nested", "selected")}))
		root, err := openSecurityHandle(dir, true, windows.READ_CONTROL)
		requirements.NoError(err)
		defer windows.CloseHandle(root) //nolint:errcheck
		requirements.NoError(verifyOwnerOnlyHandle(root, false))
		nested, err := openSecurityHandle(filepath.Dir(selected), true, windows.READ_CONTROL)
		requirements.NoError(err)
		defer windows.CloseHandle(nested) //nolint:errcheck
		requirements.NoError(verifyOwnerOnlyHandle(nested, false))
	})
	t.Run("omitted beneath selected ancestor with inherited access", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "tokens")
		require.NoError(t, os.Mkdir(dir, 0700))
		shareRestoredTestDirectory(t, dir, windows.GENERIC_READ, windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE)
		nested := filepath.Join(dir, "nested")
		require.NoError(t, os.Mkdir(nested, 0700))
		require.NoError(t, os.WriteFile(filepath.Join(nested, "selected"), []byte("selected"), 0600))
		require.NoError(t, os.WriteFile(filepath.Join(nested, "omitted"), []byte("omitted"), 0600))
		require.ErrorIs(t, SecureRestored(dir, []string{filepath.Join("nested", "selected")}), ErrRestorePermissionsConflict)
	})
	t.Run("protected selected ancestor preserves inherited omitted access", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "tokens")
		require.NoError(t, os.Mkdir(dir, 0700))
		root := shareRestoredTestDirectory(t, dir, windows.GENERIC_READ, windows.NO_INHERITANCE)
		nested := filepath.Join(dir, "nested")
		require.NoError(t, os.Mkdir(nested, 0700))
		owner := providerCredentialTestAccess(currentWindowsProviderCredentialSID(t), windows.GENERIC_ALL, windows.TRUSTEE_IS_USER)
		owner.Inheritance = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
		ancestor := setProviderCredentialTestACL(t, nested, []windows.EXPLICIT_ACCESS{owner}, true)
		require.NoError(t, verifyOwnerOnlyHandle(ancestor, false))
		require.NoError(t, os.WriteFile(filepath.Join(nested, "selected"), []byte("selected"), 0600))
		omitted := filepath.Join(nested, "omitted")
		require.NoError(t, os.WriteFile(omitted, []byte("omitted"), 0600))
		h, err := openSecurityHandle(omitted, false, windows.READ_CONTROL)
		require.NoError(t, err)
		defer windows.CloseHandle(h) //nolint:errcheck
		sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		require.NoError(t, err)
		inherited, err := inheritedAccess(sd)
		require.NoError(t, err)
		require.True(t, inherited)
		before := restoredTestDescriptor(t, omitted, false)
		require.NoError(t, SecureRestored(dir, []string{filepath.Join("nested", "selected")}))
		require.NoError(t, verifyOwnerOnlyHandle(root, false))
		assert.Equal(t, before, restoredTestDescriptor(t, omitted, false))
	})
	t.Run("kelvin sign does not match k", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "tokens")
		require.NoError(t, os.Mkdir(dir, 0700))
		shareRestoredTestDirectory(t, dir, windows.GENERIC_READ, windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "K.json"), []byte("selected"), 0600))
		omitted, err := os.OpenFile(filepath.Join(dir, "k.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if os.IsExist(err) {
			t.Skip("filesystem treats Kelvin sign and k as the same name")
		}
		require.NoError(t, err)
		require.NoError(t, omitted.Close())
		require.ErrorIs(t, SecureRestored(dir, []string{"K.json"}), ErrRestorePermissionsConflict)
	})
	t.Run("unrelated inherited entries do not block repair", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "tokens")
		require.NoError(t, (nativePermissions{}).secureDirectory(dir))
		selectedDir := filepath.Join(dir, "selected")
		otherDir := filepath.Join(dir, "other")
		require.NoError(t, os.Mkdir(selectedDir, 0700))
		require.NoError(t, os.Mkdir(otherDir, 0700))
		shareRestoredTestDirectory(t, selectedDir, windows.GENERIC_READ, windows.NO_INHERITANCE)
		owner := providerCredentialTestAccess(currentWindowsProviderCredentialSID(t), windows.GENERIC_ALL, windows.TRUSTEE_IS_USER)
		owner.Inheritance = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
		other := setProviderCredentialTestACL(t, otherDir, []windows.EXPLICIT_ACCESS{owner}, false)
		_, control := readProviderCredentialTestACL(t, other)
		require.Zero(t, control&windows.SE_DACL_PROTECTED)
		require.NoError(t, os.WriteFile(filepath.Join(selectedDir, "token"), []byte("selected"), 0600))
		omitted := filepath.Join(otherDir, "omitted")
		require.NoError(t, os.WriteFile(omitted, []byte("omitted"), 0600))
		h, err := openSecurityHandle(omitted, false, windows.READ_CONTROL)
		require.NoError(t, err)
		defer windows.CloseHandle(h) //nolint:errcheck
		sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		require.NoError(t, err)
		inherited, err := inheritedAccess(sd)
		require.NoError(t, err)
		require.True(t, inherited)
		before := restoredTestDescriptor(t, omitted, false)
		require.NoError(t, SecureRestored(dir, []string{filepath.Join("selected", "token")}))
		assert.Equal(t, before, restoredTestDescriptor(t, omitted, false))
		require.NoError(t, SecureRestored(dir, []string{filepath.Join("selected", "token")}))
		assert.Equal(t, before, restoredTestDescriptor(t, omitted, false))
	})
}
