//go:build windows

package providercredentials

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

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

	source := "source\x00credential"
	_, expectedErr = windows.UTF16PtrFromString(source)
	requirements.Error(expectedErr)
	err = replaceStoreFile(source, filepath.Join(t.TempDir(), "target"))
	requirements.Error(err)
	requirements.ErrorIs(err, expectedErr)
	assertions.Contains(err.Error(), "encode provider credential source:")

	target := "target\x00credential"
	_, expectedErr = windows.UTF16PtrFromString(target)
	requirements.Error(expectedErr)
	err = replaceStoreFile(filepath.Join(t.TempDir(), "source"), target)
	requirements.Error(err)
	requirements.ErrorIs(err, expectedErr)
	assertions.Contains(err.Error(), "encode provider credential target:")

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
				require.Equal(uint32(providerCredentialFileAllAccess), uint32(ace.Mask))
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
				require.Error(t, verifyOwnerOnlyHandleForUser(handle, user))
			} else {
				require.NoError(t, verifyOwnerOnlyHandleForUser(handle, user))
			}
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
	output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, target).CombinedOutput()
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
	handle, err := openSecurityHandle(path, false, windows.READ_CONTROL|windows.WRITE_DAC)
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
