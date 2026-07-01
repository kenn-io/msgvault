package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"go.kenn.io/kit/selfupdate"
	"go.kenn.io/msgvault/internal/config"
)

const (
	releaseOwner         = "kenn-io"
	releaseRepo          = "msgvault"
	binaryName           = "msgvault"
	cacheFileName        = "update_check.json"
	cacheDuration        = time.Hour
	devCacheDuration     = 15 * time.Minute
	metadataTimeout      = 30 * time.Second
	downloadTimeout      = 30 * time.Minute
	defaultGitHubBaseURL = "https://github.com"
)

type UpdateInfo = selfupdate.Info

type Reporter interface {
	Stepf(format string, args ...any)
	Progress(downloaded, total int64)
}

type Deps struct {
	Client             *http.Client
	Now                func() time.Time
	Version            string
	GOOS               string
	GOARCH             string
	CacheDir           func() string
	Executable         func() (string, error)
	GitHubAPIBaseURL   string
	GitHubBaseURL      string
	ReleaseManifestURL string
}

type Updater struct {
	deps Deps
}

type stdoutReporter struct {
	out        io.Writer
	progressFn func(downloaded, total int64)
}

type nopReporter struct{}

func CheckForUpdate(currentVersion string, forceCheck bool) (*UpdateInfo, error) {
	return defaultUpdater(currentVersion).CheckForUpdate(forceCheck)
}

func PerformUpdate(info *UpdateInfo, progressFn func(downloaded, total int64)) error {
	version := ""
	if info != nil {
		version = info.CurrentVersion
	}
	return defaultUpdater(version).PerformUpdate(info, stdoutReporter{
		out:        os.Stdout,
		progressFn: progressFn,
	})
}

func NewUpdater(deps Deps) *Updater {
	if deps.Client == nil {
		deps.Client = defaultHTTPClient()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Version == "" {
		deps.Version = "unknown"
	}
	if deps.GOOS == "" {
		deps.GOOS = runtime.GOOS
	}
	if deps.GOARCH == "" {
		deps.GOARCH = runtime.GOARCH
	}
	if deps.CacheDir == nil {
		deps.CacheDir = config.DefaultHome
	}
	if deps.Executable == nil {
		deps.Executable = os.Executable
	}
	if deps.GitHubBaseURL == "" {
		deps.GitHubBaseURL = defaultGitHubBaseURL
	}
	return &Updater{deps: deps}
}

func defaultUpdater(version string) *Updater {
	return NewUpdater(Deps{Version: version})
}

func (u *Updater) CheckForUpdate(forceCheck bool) (*UpdateInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
	defer cancel()
	client := u.client()
	actualVersion := u.deps.Version
	isDevBuild := selfupdate.IsDevBuildVersion(actualVersion)
	if isDevBuild {
		client.CurrentVersion = "dev"
	}
	info, err := client.Check(ctx, selfupdate.CheckOptions{
		Force:  forceCheck,
		GOOS:   u.deps.GOOS,
		GOARCH: u.deps.GOARCH,
	})
	if err != nil {
		return nil, fmt.Errorf("check for updates: %w", err)
	}
	if info != nil && isDevBuild {
		info.CurrentVersion = actualVersion
		info.IsDevBuild = true
	}
	return info, nil
}

func (u *Updater) PerformUpdate(info *UpdateInfo, reporter Reporter) error {
	reporter = normalizeReporter(reporter)
	if info == nil {
		return errors.New("update info is nil")
	}
	if info.Checksum == "" {
		return fmt.Errorf("no checksum available for %s - refusing to install unverified binary", info.AssetName)
	}

	installDir, err := u.installDir()
	if err != nil {
		return err
	}
	targetBinary := executableName(u.deps.GOOS)
	dstPath := filepath.Join(installDir, targetBinary)

	reporter.Stepf("Downloading %s...\n", info.AssetName)
	installingNotified := false
	progress := func(downloaded, total int64) {
		reporter.Progress(downloaded, total)
		if !installingNotified && total > 0 && downloaded >= total {
			reporter.Stepf("\nVerifying and installing...\n")
			installingNotified = true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()
	if err := u.client().Install(ctx, info, selfupdate.InstallOptions{
		DestinationPath:   dstPath,
		ArchiveBinaryName: targetBinary,
		Progress:          progress,
	}); err != nil {
		return fmt.Errorf("install update: %w", err)
	}
	if !installingNotified {
		reporter.Stepf("Verifying and installing...\n")
	}
	reporter.Stepf("Update complete.\n")
	return nil
}

func (u *Updater) client() selfupdate.Client {
	return selfupdate.Client{
		Owner:                  releaseOwner,
		Repo:                   releaseRepo,
		BinaryName:             binaryName,
		CurrentVersion:         u.deps.Version,
		CacheDir:               u.deps.CacheDir(),
		HTTPClient:             u.deps.Client,
		Clock:                  u.deps.Now,
		GitHubAPIBaseURL:       u.deps.GitHubAPIBaseURL,
		GitHubWebBaseURL:       u.deps.GitHubBaseURL,
		ReleaseManifestURL:     u.deps.ReleaseManifestURL,
		GitHubToken:            selfupdate.EnvironmentGitHubToken(),
		UserAgent:              "msgvault/" + u.deps.Version,
		CacheFileName:          cacheFileName,
		CacheDuration:          cacheDuration,
		DevCacheDuration:       devCacheDuration,
		AllowUnsignedChecksums: true,
	}
}

func (u *Updater) installDir() (string, error) {
	currentExe, err := u.deps.Executable()
	if err != nil {
		return "", fmt.Errorf("find current executable: %w", err)
	}
	currentExe, err = filepath.EvalSymlinks(currentExe)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks: %w", err)
	}
	return filepath.Dir(currentExe), nil
}

func executableName(goos string) string {
	if goos == "windows" {
		return binaryName + ".exe"
	}
	return binaryName
}

func IsDevBuildVersion(v string) bool {
	return selfupdate.IsDevBuildVersion(v)
}

func IsNewer(v1, v2 string) bool {
	return selfupdate.IsNewer(v1, v2)
}

func FormatSize(bytes int64) string {
	return selfupdate.FormatSize(bytes)
}

func normalizeReporter(reporter Reporter) Reporter {
	if reporter == nil {
		return nopReporter{}
	}
	return reporter
}

func (r stdoutReporter) Stepf(format string, args ...any) {
	if r.out == nil {
		return
	}
	_, _ = fmt.Fprintf(r.out, format, args...)
}

func (r stdoutReporter) Progress(downloaded, total int64) {
	if r.progressFn != nil {
		r.progressFn(downloaded, total)
	}
}

func (nopReporter) Stepf(string, ...any) {}

func (nopReporter) Progress(int64, int64) {}

func defaultHTTPClient() *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{}
	}
	cloned := transport.Clone()
	cloned.ResponseHeaderTimeout = metadataTimeout
	return &http.Client{Transport: cloned}
}
