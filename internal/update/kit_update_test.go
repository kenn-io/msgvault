package update

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/selfupdate"
)

const kitTestHash64 = "abc123def456789012345678901234567890123456789012345678901234abcd"

func TestUpdaterClientUsesKitSelfUpdateConfiguration(t *testing.T) {
	assert := assert.New(t)
	home := t.TempDir()
	u := NewUpdater(Deps{
		Version: "v0.16.0",
		CacheDir: func() string {
			return home
		},
	})

	client := u.client()

	assert.Equal("kenn-io", client.Owner, "owner")
	assert.Equal("msgvault", client.Repo, "repo")
	assert.Equal("msgvault", client.BinaryName, "binary name")
	assert.Equal("v0.16.0", client.CurrentVersion, "current version")
	assert.Equal(home, client.CacheDir, "cache dir")
	assert.Equal("msgvault/v0.16.0", client.UserAgent, "user agent")
	assert.True(client.AllowUnsignedChecksums, "preserve SHA256SUMS-only release compatibility")
	assert.Equal(
		"msgvault_0.17.0_linux_amd64.tar.gz",
		selfupdate.DefaultAssetName(selfupdate.AssetRequest{
			BinaryName: "msgvault",
			Version:    "0.17.0",
			GOOS:       "linux",
			GOARCH:     "amd64",
			Extension:  ".tar.gz",
		}),
		"release asset naming must remain compatible with 0.16.x updater")
}

func TestUpdaterCheckForUpdateUsesKitConventionalReleaseDiscovery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const (
		currentVersion = "v0.16.0"
		latestTag      = "v0.17.0"
		assetName      = "msgvault_0.17.0_linux_amd64.tar.gz"
	)
	var requests []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/kenn-io/msgvault/releases/latest":
			http.Redirect(w, r, "/kenn-io/msgvault/releases/tag/"+latestTag, http.StatusFound)
		case "/kenn-io/msgvault/releases/tag/" + latestTag:
			w.WriteHeader(http.StatusOK)
		case "/kenn-io/msgvault/releases/download/" + latestTag + "/" + assetName:
			w.Header().Set("Content-Length", "123")
			w.WriteHeader(http.StatusOK)
		case "/kenn-io/msgvault/releases/download/" + latestTag + "/SHA256SUMS":
			_, _ = fmt.Fprintf(w, "%s  %s\n", kitTestHash64, assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	u := NewUpdater(Deps{
		Client:        server.Client(),
		Version:       currentVersion,
		GOOS:          "linux",
		GOARCH:        "amd64",
		GitHubBaseURL: server.URL,
		CacheDir: func() string {
			return t.TempDir()
		},
	})

	info, err := u.CheckForUpdate(true)
	require.NoError(err, "CheckForUpdate")
	require.NotNil(info, "update info")

	assert.Equal(currentVersion, info.CurrentVersion, "current version")
	assert.Equal(latestTag, info.LatestVersion, "latest version")
	assert.Equal(assetName, info.AssetName, "asset name")
	assert.Equal(server.URL+"/kenn-io/msgvault/releases/download/"+latestTag+"/"+assetName, info.DownloadURL, "download URL")
	assert.Equal(int64(123), info.Size, "asset size")
	assert.Equal(kitTestHash64, info.Checksum, "checksum")
	assert.Equal([]string{
		"GET /kenn-io/msgvault/releases/latest",
		"GET /kenn-io/msgvault/releases/tag/" + latestTag,
		"HEAD /kenn-io/msgvault/releases/download/" + latestTag + "/" + assetName,
		"HEAD /kenn-io/msgvault/releases/download/" + latestTag + "/" + assetName + ".sha256.sig",
		"HEAD /kenn-io/msgvault/releases/download/" + latestTag + "/" + assetName + ".sig",
		"GET /kenn-io/msgvault/releases/download/" + latestTag + "/SHA256SUMS",
	}, requests)
}

func TestUpdaterCheckForUpdateOffersSameBaseReleaseForDevBuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const (
		currentVersion = "0.17.0-1-gabcdef"
		latestTag      = "v0.17.0"
		assetName      = "msgvault_0.17.0_linux_amd64.tar.gz"
	)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kenn-io/msgvault/releases/latest":
			http.Redirect(w, r, "/kenn-io/msgvault/releases/tag/"+latestTag, http.StatusFound)
		case "/kenn-io/msgvault/releases/tag/" + latestTag:
			w.WriteHeader(http.StatusOK)
		case "/kenn-io/msgvault/releases/download/" + latestTag + "/" + assetName:
			w.Header().Set("Content-Length", "123")
			w.WriteHeader(http.StatusOK)
		case "/kenn-io/msgvault/releases/download/" + latestTag + "/SHA256SUMS":
			_, _ = fmt.Fprintf(w, "%s  %s\n", kitTestHash64, assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	u := NewUpdater(Deps{
		Client:        server.Client(),
		Version:       currentVersion,
		GOOS:          "linux",
		GOARCH:        "amd64",
		GitHubBaseURL: server.URL,
		CacheDir: func() string {
			return t.TempDir()
		},
	})

	info, err := u.CheckForUpdate(true)
	require.NoError(err, "CheckForUpdate")
	require.NotNil(info, "dev build should be offered latest official release")
	assert.Equal(currentVersion, info.CurrentVersion, "current version")
	assert.Equal(latestTag, info.LatestVersion, "latest version")
	assert.True(info.IsDevBuild, "dev build")
}

func TestFormatAndVersionHelpersDelegateToKit(t *testing.T) {
	assert := assert.New(t)

	assert.Equal("1.5 KB", FormatSize(1536), "format size")
	assert.True(IsDevBuildVersion("0.16.1-2-g75d300a"), "git describe build")
	assert.False(IsDevBuildVersion("v0.16.1-rc1"), "release prerelease")
	assert.True(IsNewer("0.17.0", "0.16.1-2-g75d300a"), "release newer than git describe base")
	assert.False(IsNewer("0.16.0", "0.16.1-2-g75d300a"), "older than git describe base")
}

func TestPerformUpdateInstallsWithKitClient(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmp := t.TempDir()
	srcArchive := filepath.Join(tmp, "msgvault_0.17.0_linux_amd64.tar.gz")
	dstPath := filepath.Join(tmp, "bin", "msgvault")
	require.NoError(makeTestTarGz(srcArchive, "msgvault", "new binary"), "write archive")
	checksum, err := selfupdate.HashFile(srcArchive)
	require.NoError(err, "hash archive")
	archiveBytes, err := os.ReadFile(srcArchive)
	require.NoError(err, "read archive")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(archiveBytes)))
		_, _ = w.Write(archiveBytes)
	}))
	t.Cleanup(server.Close)
	require.NoError(os.MkdirAll(filepath.Dir(dstPath), 0o755), "create install dir")
	require.NoError(os.WriteFile(dstPath, []byte("old binary"), 0o755), "write current binary")

	u := NewUpdater(Deps{
		Client:  server.Client(),
		Version: "v0.16.0",
		GOOS:    "linux",
		GOARCH:  "amd64",
		Executable: func() (string, error) {
			return dstPath, nil
		},
		CacheDir: func() string {
			return tmp
		},
	})

	err = u.PerformUpdate(&UpdateInfo{
		Owner:         "kenn-io",
		Repo:          "msgvault",
		LatestVersion: "v0.17.0",
		AssetName:     "msgvault_0.17.0_linux_amd64.tar.gz",
		DownloadURL:   server.URL + "/msgvault_0.17.0_linux_amd64.tar.gz",
		GOOS:          "linux",
		GOARCH:        "amd64",
		Checksum:      checksum,
		Size:          int64(len(archiveBytes)),
	}, nopReporter{})

	require.NoError(err, "PerformUpdate")
	got, err := os.ReadFile(dstPath)
	require.NoError(err, "read installed binary")
	assert.Equal("new binary", string(got), "installed binary")
}

func TestDefaultUpdaterUsesMSGVAULTHomeForCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MSGVAULT_HOME", home)

	client := defaultUpdater("v0.16.0").client()

	assert.Equal(t, home, client.CacheDir, "cache dir")
}

func TestUpdaterCheckForUpdateUsesCache(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	home := t.TempDir()
	cache := struct {
		CheckedAt time.Time `json:"checked_at"`
		Version   string    `json:"version"`
	}{
		CheckedAt: time.Unix(1000, 0),
		Version:   "v0.17.0",
	}
	data, err := json.Marshal(cache)
	require.NoError(err, "marshal cache")
	require.NoError(os.WriteFile(filepath.Join(home, "update_check.json"), data, 0o600), "write cache")
	u := NewUpdater(Deps{
		Version: "0.16.1-2-g75d300a",
		Now: func() time.Time {
			return time.Unix(1000, 0)
		},
		CacheDir: func() string {
			return home
		},
	})

	info, err := u.CheckForUpdate(false)
	require.NoError(err, "CheckForUpdate")
	require.NotNil(info, "cached dev build update")
	assert.True(info.NeedsRefetch(), "cache-only result requires fresh install metadata")
	assert.Equal("v0.17.0", info.LatestVersion, "latest version")
}

func makeTestTarGz(path, name, content string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o755,
		Size: int64(len(content)),
	}); err != nil {
		return fmt.Errorf("write tar header: %w", err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		return fmt.Errorf("write tar content: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar writer: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("close gzip writer: %w", err)
	}
	return nil
}
