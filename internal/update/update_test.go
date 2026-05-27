package update

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

const (
	testHash64   = "abc123def456789012345678901234567890123456789012345678901234abcd"
	testHashAAAA = "abc123def456789012345678901234567890123456789012345678901234aaaa"
	testHashBBBB = "abc123def456789012345678901234567890123456789012345678901234bbbb"
)

func TestSanitizeTarPath(t *testing.T) {
	t.Parallel()
	destDir := t.TempDir()

	tests := []struct {
		name      string
		path      string
		wantErr   bool
		skipOnWin bool
	}{
		{"normal file", "msgvault", false, false},
		{"nested file", "bin/msgvault", false, false},
		{"absolute path Unix", "/etc/passwd", true, false},
		{"path traversal with ..", "../../../etc/passwd", true, false},
		{"path traversal mid-path", "foo/../../../etc/passwd", true, false},
		{"hidden traversal", "foo/bar/../../..", true, false},
		{"dot only", ".", false, false},
		{"double dot only", "..", true, false},
		{"empty path", "", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.skipOnWin && runtime.GOOS == "windows" {
				t.Skip("Unix-style absolute path not applicable on Windows")
			}
			_, err := sanitizeTarPath(destDir, tt.path)
			if tt.wantErr {
				assertpkg.Errorf(t, err, "sanitizeTarPath(%q)", tt.path)
			} else {
				assertpkg.NoErrorf(t, err, "sanitizeTarPath(%q)", tt.path)
			}
		})
	}
}

func TestExtractTarGzPathTraversal(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "malicious.tar.gz")
	extractDir := filepath.Join(tmpDir, "extract")
	outsideFile := filepath.Join(tmpDir, "pwned")

	testutil.CreateTarGz(t, archivePath, []testutil.ArchiveEntry{
		{Name: "../pwned", Content: "owned"},
	})

	err := extractTarGz(archivePath, extractDir)
	requirepkg.Error(t, err, "extractTarGz should fail with path traversal attempt")

	testutil.MustNotExist(t, outsideFile)
}

func TestExtractTarGzSymlinkSkipped(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "symlink.tar.gz")
	extractDir := filepath.Join(tmpDir, "extract")

	testutil.CreateTarGz(t, archivePath, []testutil.ArchiveEntry{
		{Name: "evil-link", TypeFlag: tar.TypeSymlink, LinkName: "/etc/passwd"},
		{Name: "normal.txt", Content: "test"},
	})

	requirepkg.NoError(t, extractTarGz(archivePath, extractDir), "extractTarGz failed")

	testutil.MustExist(t, filepath.Join(extractDir, "normal.txt"))
	testutil.MustNotExist(t, filepath.Join(extractDir, "evil-link"))
}

func TestExtractZip(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "test.zip")
	extractDir := filepath.Join(tmpDir, "extract")

	testutil.CreateZip(t, archivePath, []testutil.ArchiveEntry{
		{Name: "msgvault.exe", Content: "binary content"},
		{Name: "README.md", Content: "readme"},
	})

	requirepkg.NoError(t, extractZip(archivePath, extractDir), "extractZip failed")

	testutil.MustExist(t, filepath.Join(extractDir, "msgvault.exe"))
	testutil.MustExist(t, filepath.Join(extractDir, "README.md"))

	content, err := os.ReadFile(filepath.Join(extractDir, "msgvault.exe"))
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, "binary content", string(content))
}

func TestExtractZipPathTraversal(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "malicious.zip")
	extractDir := filepath.Join(tmpDir, "extract")
	outsideFile := filepath.Join(tmpDir, "pwned")

	testutil.CreateZip(t, archivePath, []testutil.ArchiveEntry{
		{Name: "../pwned", Content: "owned"},
	})

	err := extractZip(archivePath, extractDir)
	requirepkg.Error(t, err, "extractZip should fail with path traversal attempt")

	testutil.MustNotExist(t, outsideFile)
}

func TestExtractChecksum(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		assetName string
		want      string
	}{
		{
			name:      "standard sha256sum format",
			body:      testHash64 + "  msgvault_darwin_arm64.tar.gz",
			assetName: "msgvault_darwin_arm64.tar.gz",
			want:      testHash64,
		},
		{
			name:      "uppercase checksum",
			body:      "ABC123DEF456789012345678901234567890123456789012345678901234ABCD  msgvault_linux_amd64.tar.gz",
			assetName: "msgvault_linux_amd64.tar.gz",
			want:      testHash64,
		},
		{
			name:      "multiline with target in middle",
			body:      fmt.Sprintf("%s  msgvault_linux_amd64.tar.gz\n%s  msgvault_darwin_arm64.tar.gz", testHashAAAA, testHashBBBB),
			assetName: "msgvault_darwin_arm64.tar.gz",
			want:      testHashBBBB,
		},
		{
			name:      "no match",
			body:      testHash64 + "  msgvault_linux_amd64.tar.gz",
			assetName: "msgvault_darwin_arm64.tar.gz",
			want:      "",
		},
		{
			name:      "empty body",
			body:      "",
			assetName: "msgvault_darwin_arm64.tar.gz",
			want:      "",
		},
		{
			name:      "substring filename should not match",
			body:      testHash64 + "  msgvault_darwin_arm64.tar.gz.sig",
			assetName: "msgvault_darwin_arm64.tar.gz",
			want:      "",
		},
		{
			name:      "exact match with superset also present",
			body:      fmt.Sprintf("%s  msgvault_darwin_arm64.tar.gz.sig\n%s  msgvault_darwin_arm64.tar.gz", testHashAAAA, testHashBBBB),
			assetName: "msgvault_darwin_arm64.tar.gz",
			want:      testHashBBBB,
		},
		{
			name:      "binary mode star prefix",
			body:      testHash64 + " *msgvault_darwin_arm64.tar.gz",
			assetName: "msgvault_darwin_arm64.tar.gz",
			want:      testHash64,
		},
		{
			name:      "trailing comment ignored",
			body:      testHash64 + "  msgvault_darwin_arm64.tar.gz  # some comment",
			assetName: "msgvault_darwin_arm64.tar.gz",
			want:      testHash64,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractChecksum(tt.body, tt.assetName)
			assertpkg.Equal(t, tt.want, got)
		})
	}
}

func TestExtractBaseSemver(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		want    string
	}{
		{"0.1.0", "0.1.0"},
		{"1.2.3", "1.2.3"},
		{"v0.1.0", "0.1.0"},
		{"0.4.0-5-gabcdef", "0.4.0"},
		{"v0.4.0-5-gabcdef", "0.4.0"},
		{"0.4.0-dev", "0.4.0"},
		{"0.4.0-rc1", "0.4.0"},
		{"dev", ""},
		{"abc1234", ""},
		{"88be010", ""},
		{"", ""},
		{"0", ""},
		{"v", ""},
		{"1.0", "1.0"},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			t.Parallel()
			assertpkg.Equal(t, tt.want, extractBaseSemver(tt.version))
		})
	}
}

func TestIsDevBuildVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		want    bool
	}{
		{"0.1.0", false},
		{"v0.1.0", false},
		{"1.0.0", false},
		{"0.16.1-2-g75d300a", true},
		{"v0.16.1-2-g75d300a", true},
		{"0.4.0-5-gabcdef-dirty", true},
		{"dev", true},
		{"abc1234", true},
		{"0.16.1-rc1", false},
		{"v1.0.0-beta.1", false},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			t.Parallel()
			assertpkg.Equal(t, tt.want, isDevBuildVersion(tt.version))
		})
	}
}

func TestNormalizePrereleaseIdentifiers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		prerelease string
		want       string
	}{
		{"simple rc with number", "rc10", "rc.10"},
		{"simple beta with number", "beta2", "beta.2"},
		{"alpha with number", "alpha1", "alpha.1"},
		{"multi-part rc10.1 normalizes to rc.10.1", "rc10.1", "rc.10.1"},
		{"mixed identifiers alpha10.beta2", "alpha10.beta2", "alpha.10.beta.2"},
		{"alphanumeric suffix rc10a stays unchanged", "rc10a", "rc10a"},
		{"leading zeros rc01 stays unchanged", "rc01", "rc01"},
		{"leading zeros beta007 stays unchanged", "beta007", "beta007"},
		{"pure numeric stays unchanged", "1", "1"},
		{"pure numeric multi stays unchanged", "1.2.3", "1.2.3"},
		{"already dotted rc.10 stays unchanged", "rc.10", "rc.10"},
		{"no number suffix stays unchanged", "alpha", "alpha"},
		{"empty string", "", ""},
		{"complex mixed", "pre10.rc2.beta05", "pre.10.rc.2.beta05"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizePrereleaseIdentifiers(tt.prerelease)
			assertpkg.Equal(t, tt.want, got)
		})
	}
}

func TestIsNewer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		v1, v2 string
		want   bool
	}{
		{"major version bump", "1.0.0", "0.9.0", true},
		{"minor version bump", "1.1.0", "1.0.0", true},
		{"patch version bump", "1.0.1", "1.0.0", true},
		{"major boundary crossing", "2.0.0", "1.9.9", true},
		{"same version not newer", "1.0.0", "1.0.0", false},
		{"older version not newer", "0.9.0", "1.0.0", false},
		{"v prefix handled", "v1.0.0", "v0.9.0", true},
		{"release vs non-semver hash", "0.4.2", "88be010", false},
		{"release vs dev string", "0.4.2", "dev", false},
		{"bad version not newer", "badversion", "0.4.0", false},
		{"same base as dev build not newer", "0.4.0", "0.4.0-5-gabcdef", false},
		{"higher minor than dev build", "0.5.0", "0.4.0-5-gabcdef", true},
		{"higher patch than dev build", "0.4.1", "0.4.0-5-gabcdef", true},
		{"lower version than dev build", "0.3.0", "0.4.0-5-gabcdef", false},
		{"higher minor than prerelease", "0.5.0", "0.4.0-rc1", true},
		{"release newer than its prerelease", "0.4.0", "0.4.0-rc1", true},
		{"prerelease not newer than release", "0.4.0-rc1", "0.4.0", false},
		{"rc2 newer than rc1", "0.4.0-rc2", "0.4.0-rc1", true},
		// Non-dotted prerelease identifiers are normalized for numeric comparison
		// (e.g., "rc10" -> "rc.10") so rc10 > rc2 as expected.
		{"non-dotted prerelease comparison rc10 vs rc2", "0.4.0-rc10", "0.4.0-rc2", true},
		{"non-dotted prerelease comparison rc2 vs rc10", "0.4.0-rc2", "0.4.0-rc10", false},
		{"non-dotted prerelease beta10 vs beta2", "0.4.0-beta10", "0.4.0-beta2", true},
		{"rc newer than beta lexicographically", "0.4.0-rc1", "0.4.0-beta1", true},
		{"alpha older than beta", "0.4.0-alpha1", "0.4.0-beta1", false},
		{"dotted prerelease numeric comparison rc.10 vs rc.2", "0.4.0-rc.10", "0.4.0-rc.2", true},
		{"dotted prerelease comparison", "0.4.0-rc.2", "0.4.0-rc.1", true},
		{"numeric segment less than non-numeric", "0.4.0-1", "0.4.0-rc1", false},
		{"non-numeric greater than numeric", "0.4.0-rc1", "0.4.0-1", true},
		{"prerelease of higher base beats lower release", "0.4.0-beta1", "0.3.9", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertpkg.Equal(t, tt.want, isNewer(tt.v1, tt.v2))
		})
	}
}

func TestResolveLatestTag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		status     int
		location   string
		wantTag    string
		wantErrSub string
	}{
		{
			name:     "valid 302 redirect",
			status:   http.StatusFound,
			location: "https://github.com/wesm/msgvault/releases/tag/v0.30.1",
			wantTag:  "v0.30.1",
		},
		{
			name:     "pre-release tag",
			status:   http.StatusFound,
			location: "https://github.com/wesm/msgvault/releases/tag/v0.9.0-rc1",
			wantTag:  "v0.9.0-rc1",
		},
		{
			name:       "200 OK is not a redirect",
			status:     http.StatusOK,
			wantErrSub: "expected redirect",
		},
		{
			name:       "redirect target without /tag/",
			status:     http.StatusFound,
			location:   "https://github.com/wesm/msgvault/releases",
			wantErrSub: "unexpected redirect target",
		},
		{
			name:       "empty tag after /tag/",
			status:     http.StatusFound,
			location:   "https://github.com/wesm/msgvault/releases/tag/",
			wantErrSub: "empty tag",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := requirepkg.New(t)
			assert := assertpkg.New(t)
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					if tt.location != "" {
						w.Header().Set("Location", tt.location)
					}
					w.WriteHeader(tt.status)
				},
			))
			defer srv.Close()

			tag, err := resolveLatestTag(srv.URL)
			if tt.wantErrSub != "" {
				require.ErrorContains(err, tt.wantErrSub)
				return
			}
			require.NoError(err, "unexpected error")
			assert.Equal(tt.wantTag, tag)
		})
	}
}

func TestFetchContentLength(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		status     int
		bodySize   int
		wantSize   int64
		wantErrSub string
	}{
		{
			name:     "200 with body",
			status:   http.StatusOK,
			bodySize: 1234,
			wantSize: 1234,
		},
		{
			name:       "404",
			status:     http.StatusNotFound,
			wantErrSub: "404",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := requirepkg.New(t)
			assert := assertpkg.New(t)
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					if tt.bodySize > 0 {
						w.Header().Set("Content-Length", strconv.Itoa(tt.bodySize))
					}
					w.WriteHeader(tt.status)
				},
			))
			defer srv.Close()

			size, err := fetchContentLength(srv.URL)
			if tt.wantErrSub != "" {
				require.ErrorContains(err, tt.wantErrSub)
				return
			}
			require.NoError(err, "unexpected error")
			assert.Equal(tt.wantSize, size)
		})
	}
}

func TestFormatSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{100, "100 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{10485760, "10.0 MB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			assertpkg.Equal(t, tt.want, FormatSize(tt.bytes))
		})
	}
}

func TestCheckCache(t *testing.T) {
	tests := []struct {
		name           string
		currentVersion string
		cleanVersion   string
		isDevBuild     bool
		cachedVersion  string
		cacheAge       time.Duration
		wantInfo       bool // whether UpdateInfo is returned
		wantDone       bool // whether cache result should be used
		wantIsDevBuild bool
	}{
		{
			name:           "valid cache no update available",
			currentVersion: "v1.0.0",
			cleanVersion:   "1.0.0",
			isDevBuild:     false,
			cachedVersion:  "v1.0.0",
			cacheAge:       30 * time.Minute,
			wantInfo:       false,
			wantDone:       true,
		},
		{
			name:           "valid cache update available triggers fresh fetch",
			currentVersion: "v1.0.0",
			cleanVersion:   "1.0.0",
			isDevBuild:     false,
			cachedVersion:  "v1.1.0",
			cacheAge:       30 * time.Minute,
			wantInfo:       false,
			wantDone:       false, // Need fresh data for download info
		},
		{
			name:           "dev build always returns update info",
			currentVersion: "0.16.1-2-g75d300a",
			cleanVersion:   "0.16.1-2-g75d300a",
			isDevBuild:     true,
			cachedVersion:  "v1.0.0",
			cacheAge:       5 * time.Minute,
			wantInfo:       true,
			wantDone:       true,
			wantIsDevBuild: true,
		},
		{
			name:           "expired cache for release build",
			currentVersion: "v1.0.0",
			cleanVersion:   "1.0.0",
			isDevBuild:     false,
			cachedVersion:  "v1.0.0",
			cacheAge:       2 * time.Hour, // > 1 hour cache duration
			wantInfo:       false,
			wantDone:       false,
		},
		{
			name:           "expired cache for dev build",
			currentVersion: "0.16.1-2-g75d300a",
			cleanVersion:   "0.16.1-2-g75d300a",
			isDevBuild:     true,
			cachedVersion:  "v1.0.0",
			cacheAge:       20 * time.Minute, // > 15 minute dev cache duration
			wantInfo:       false,
			wantDone:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := requirepkg.New(t)
			assert := assertpkg.New(t)
			tmpDir := t.TempDir()
			t.Setenv("MSGVAULT_HOME", tmpDir)

			// Write cache file with specified age
			cached := cachedCheck{
				CheckedAt: time.Now().Add(-tt.cacheAge),
				Version:   tt.cachedVersion,
			}
			data, err := json.Marshal(cached)
			require.NoError(err, "failed to marshal cache")
			cachePath := filepath.Join(tmpDir, cacheFileName)
			require.NoError(os.WriteFile(cachePath, data, 0600), "failed to write cache")

			info, done := checkCache(tt.currentVersion, tt.cleanVersion, tt.isDevBuild)

			assert.Equal(tt.wantDone, done)
			if tt.wantInfo {
				require.NotNil(info, "expected UpdateInfo to be non-nil")
				assert.Equal(tt.wantIsDevBuild, info.IsDevBuild)
				assert.Equal(tt.currentVersion, info.CurrentVersion)
				assert.Equal(tt.cachedVersion, info.LatestVersion)
			} else {
				assert.Nilf(info, "expected UpdateInfo to be nil, got %+v", info)
			}
		})
	}
}

func TestCheckCacheNoFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	// No cache file exists
	info, done := checkCache("v1.0.0", "1.0.0", false)

	assertpkg.False(t, done)
	assertpkg.Nilf(t, info, "expected UpdateInfo to be nil, got %+v", info)
}

// TestSaveCacheFilePermissions verifies that the update check cache file is
// saved with restrictive permissions (0600) to protect user data.
func TestSaveCacheFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions not enforced on Windows")
	}

	// Use a temp directory as MSGVAULT_HOME to avoid touching real user data
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	// Call saveCache which writes to getCacheDir()/update_check.json
	saveCache("1.0.0")

	cachePath := filepath.Join(tmpDir, cacheFileName)
	info, err := os.Stat(cachePath)
	requirepkg.NoErrorf(t, err, "Stat(%s)", cachePath)

	// File should have 0600 permissions (owner read/write only)
	assertpkg.Equal(t, os.FileMode(0600), info.Mode().Perm(), "cache file permissions")
}

func TestInstallBinaryTo(t *testing.T) {
	t.Parallel()

	t.Run("successful installation", func(t *testing.T) {
		require := requirepkg.New(t)
		assert := assertpkg.New(t)
		t.Parallel()
		tmpDir := t.TempDir()

		// Create source binary
		srcPath := filepath.Join(tmpDir, "new_binary")
		require.NoError(os.WriteFile(srcPath, []byte("new content"), 0644), "failed to create source")

		// Create existing binary to be replaced
		dstPath := filepath.Join(tmpDir, "msgvault")
		require.NoError(os.WriteFile(dstPath, []byte("old content"), 0755), "failed to create existing binary")

		// Install
		require.NoError(installBinaryTo(srcPath, dstPath), "installBinaryTo failed")

		// Verify new content
		content, err := os.ReadFile(dstPath)
		require.NoError(err, "failed to read installed binary")
		assert.Equal("new content", string(content))

		// Verify backup was cleaned up
		backupPath := dstPath + ".old"
		testutil.MustNotExist(t, backupPath)

		// Verify permissions (Windows does not support Unix permissions)
		if runtime.GOOS != "windows" {
			info, err := os.Stat(dstPath)
			require.NoError(err, "Stat failed")
			assert.Equal(os.FileMode(0755), info.Mode().Perm(), "permissions")
		}
	})

	t.Run("installation to new location without existing binary", func(t *testing.T) {
		require := requirepkg.New(t)
		t.Parallel()
		tmpDir := t.TempDir()

		// Create source binary
		srcPath := filepath.Join(tmpDir, "new_binary")
		require.NoError(os.WriteFile(srcPath, []byte("new content"), 0644), "failed to create source")

		// No existing binary at destination
		dstPath := filepath.Join(tmpDir, "msgvault")

		// Install
		require.NoError(installBinaryTo(srcPath, dstPath), "installBinaryTo failed")

		// Verify new content
		content, err := os.ReadFile(dstPath)
		require.NoError(err, "failed to read installed binary")
		assertpkg.Equal(t, "new content", string(content))
	})

	t.Run("backup restored on copy failure", func(t *testing.T) {
		require := requirepkg.New(t)
		if runtime.GOOS == "windows" {
			t.Skip("POSIX file permissions not enforced on Windows")
		}
		t.Parallel()
		tmpDir := t.TempDir()

		// Create source binary but make it unreadable to cause copy to fail
		// after the backup rename succeeds
		srcPath := filepath.Join(tmpDir, "new_binary")
		require.NoError(os.WriteFile(srcPath, []byte("new content"), 0000), "failed to create source")
		t.Cleanup(func() {
			_ = os.Chmod(srcPath, 0644) // Restore for cleanup
		})

		// Create existing binary
		dstPath := filepath.Join(tmpDir, "msgvault")
		require.NoError(os.WriteFile(dstPath, []byte("old content"), 0755), "failed to create existing binary")

		// Attempt install - should fail during copy (not rename)
		err := installBinaryTo(srcPath, dstPath)
		require.Error(err, "expected installBinaryTo to fail with unreadable source")

		// Verify original was restored from backup
		content, err := os.ReadFile(dstPath)
		require.NoError(err, "failed to read restored binary")
		assertpkg.Equal(t, "old content", string(content))
	})

	t.Run("stale backup removed before install", func(t *testing.T) {
		require := requirepkg.New(t)
		t.Parallel()
		tmpDir := t.TempDir()

		// Create source binary
		srcPath := filepath.Join(tmpDir, "new_binary")
		require.NoError(os.WriteFile(srcPath, []byte("new content"), 0644), "failed to create source")

		// Create existing binary
		dstPath := filepath.Join(tmpDir, "msgvault")
		require.NoError(os.WriteFile(dstPath, []byte("current content"), 0755), "failed to create existing binary")

		// Create stale backup from previous update
		backupPath := dstPath + ".old"
		require.NoError(os.WriteFile(backupPath, []byte("stale backup"), 0755), "failed to create stale backup")

		// Install
		require.NoError(installBinaryTo(srcPath, dstPath), "installBinaryTo failed")

		// Verify stale backup is gone
		testutil.MustNotExist(t, backupPath)

		// Verify new content installed
		content, err := os.ReadFile(dstPath)
		require.NoError(err, "failed to read installed binary")
		assertpkg.Equal(t, "new content", string(content))
	})
}

func TestInstallFromArchiveTo(t *testing.T) {
	t.Parallel()

	binaryName := "msgvault"
	if runtime.GOOS == "windows" {
		binaryName = "msgvault.exe"
	}

	t.Run("zip happy path", func(t *testing.T) {
		require := requirepkg.New(t)
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "test.zip")
		binaryContent := "fake binary zip"

		testutil.CreateZip(t, archivePath, []testutil.ArchiveEntry{
			{Name: binaryName, Content: binaryContent},
			{Name: "README.md", Content: "readme"},
		})

		checksum, err := hashFile(archivePath)
		require.NoError(err, "hashFile")

		dstDir := filepath.Join(tmpDir, "dest")
		require.NoError(os.MkdirAll(dstDir, 0755))
		dstPath := filepath.Join(dstDir, binaryName)

		require.NoError(installFromArchiveTo(archivePath, checksum, dstPath), "installFromArchiveTo")

		content, err := os.ReadFile(dstPath)
		require.NoError(err, "read installed binary")
		assertpkg.Equal(t, binaryContent, string(content))
	})

	t.Run("tar.gz happy path", func(t *testing.T) {
		require := requirepkg.New(t)
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "test.tar.gz")
		binaryContent := "fake binary targz"

		testutil.CreateTarGz(t, archivePath, []testutil.ArchiveEntry{
			{Name: binaryName, Content: binaryContent},
		})

		checksum, err := hashFile(archivePath)
		require.NoError(err, "hashFile")

		dstDir := filepath.Join(tmpDir, "dest")
		require.NoError(os.MkdirAll(dstDir, 0755))
		dstPath := filepath.Join(dstDir, binaryName)

		require.NoError(installFromArchiveTo(archivePath, checksum, dstPath), "installFromArchiveTo")

		content, err := os.ReadFile(dstPath)
		require.NoError(err, "read installed binary")
		assertpkg.Equal(t, binaryContent, string(content))
	})

	t.Run("checksum mismatch", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "test.zip")

		testutil.CreateZip(t, archivePath, []testutil.ArchiveEntry{
			{Name: binaryName, Content: "content"},
		})

		wrongChecksum := "0000000000000000000000000000000000000000000000000000000000000000"
		dstPath := filepath.Join(tmpDir, "dest", binaryName)

		err := installFromArchiveTo(archivePath, wrongChecksum, dstPath)
		requirepkg.Error(t, err, "expected error for checksum mismatch")
		assertpkg.Containsf(t, err.Error(), "checksum mismatch", "error should contain 'checksum mismatch'")
	})

	t.Run("empty checksum", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "test.zip")

		testutil.CreateZip(t, archivePath, []testutil.ArchiveEntry{
			{Name: binaryName, Content: "content"},
		})

		dstPath := filepath.Join(tmpDir, "dest", binaryName)

		err := installFromArchiveTo(archivePath, "", dstPath)
		requirepkg.Error(t, err, "expected error for empty checksum")
	})

	t.Run("missing binary in archive", func(t *testing.T) {
		require := requirepkg.New(t)
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "test.zip")

		testutil.CreateZip(t, archivePath, []testutil.ArchiveEntry{
			{Name: "README.md", Content: "no binary here"},
		})

		checksum, err := hashFile(archivePath)
		require.NoError(err, "hashFile")

		dstDir := filepath.Join(tmpDir, "dest")
		require.NoError(os.MkdirAll(dstDir, 0755))
		dstPath := filepath.Join(dstDir, binaryName)

		err = installFromArchiveTo(archivePath, checksum, dstPath)
		require.Error(err, "expected error for missing binary")
		assertpkg.Containsf(t, err.Error(), "not found", "error should contain 'not found'")
	})

	t.Run("overwrites existing binary", func(t *testing.T) {
		require := requirepkg.New(t)
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "test.zip")
		newContent := "new binary content"

		testutil.CreateZip(t, archivePath, []testutil.ArchiveEntry{
			{Name: binaryName, Content: newContent},
		})

		checksum, err := hashFile(archivePath)
		require.NoError(err, "hashFile")

		dstDir := filepath.Join(tmpDir, "dest")
		require.NoError(os.MkdirAll(dstDir, 0755))
		dstPath := filepath.Join(dstDir, binaryName)

		// Pre-existing binary
		require.NoError(os.WriteFile(dstPath, []byte("old content"), 0755))

		require.NoError(installFromArchiveTo(archivePath, checksum, dstPath), "installFromArchiveTo")

		content, err := os.ReadFile(dstPath)
		require.NoError(err, "read installed binary")
		assertpkg.Equal(t, newContent, string(content))

		// Verify backup was cleaned up
		testutil.MustNotExist(t, dstPath+".old")
	})
}
