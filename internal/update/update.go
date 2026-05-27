package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
	"golang.org/x/mod/semver"
)

const (
	// githubLatestReleaseURL is the HTML endpoint that 302-redirects to
	// /releases/tag/<tag>. Unlike api.github.com it is not rate-limited
	// at 60 req/hr per IP for unauthenticated callers.
	githubLatestReleaseURL    = "https://github.com/wesm/msgvault/releases/latest"
	githubReleaseDownloadBase = "https://github.com/wesm/msgvault/releases/download"
	updateUserAgent           = "msgvault-update"
	cacheFileName             = "update_check.json"
	cacheDuration             = 1 * time.Hour
	devCacheDuration          = 15 * time.Minute
)

// UpdateInfo contains information about an available update.
type UpdateInfo struct {
	CurrentVersion string
	LatestVersion  string
	DownloadURL    string
	AssetName      string
	Size           int64
	Checksum       string
	IsDevBuild     bool
}

// cachedCheck stores the last update check result.
type cachedCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	Version   string    `json:"version"`
}

// CheckForUpdate checks if a newer version is available.
// Uses a 1-hour cache to avoid hitting the GitHub API too often.
func CheckForUpdate(currentVersion string, forceCheck bool) (*UpdateInfo, error) {
	cleanVersion := strings.TrimPrefix(currentVersion, "v")
	isDevBuild := isDevBuildVersion(cleanVersion)

	if !forceCheck {
		if info, done := checkCache(currentVersion, cleanVersion, isDevBuild); done {
			return info, nil
		}
	}

	tag, err := resolveLatestTag(githubLatestReleaseURL)
	if err != nil {
		return nil, fmt.Errorf("check for updates: %w", err)
	}

	saveCache(tag)

	latestVersion := strings.TrimPrefix(tag, "v")

	if !isDevBuild && !isNewer(latestVersion, cleanVersion) {
		return nil, nil
	}

	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	assetName := fmt.Sprintf("msgvault_%s_%s_%s%s", latestVersion, runtime.GOOS, runtime.GOARCH, ext)
	downloadURL := fmt.Sprintf("%s/%s/%s", githubReleaseDownloadBase, tag, assetName)
	checksumsURL := fmt.Sprintf("%s/%s/SHA256SUMS", githubReleaseDownloadBase, tag)

	// HEAD the asset to confirm it exists for this platform. The previous
	// API-based code returned "no release asset" up front; now that we
	// construct the URL ourselves, we have to verify it resolves.
	size, err := fetchContentLength(downloadURL)
	if err != nil {
		return nil, fmt.Errorf("no release asset for %s/%s: %w", runtime.GOOS, runtime.GOARCH, err)
	}

	checksum, _ := fetchChecksumFromFile(checksumsURL, assetName)

	return &UpdateInfo{
		CurrentVersion: currentVersion,
		LatestVersion:  tag,
		DownloadURL:    downloadURL,
		AssetName:      assetName,
		Size:           size,
		Checksum:       checksum,
		IsDevBuild:     isDevBuild,
	}, nil
}

// PerformUpdate downloads and installs the update.
func PerformUpdate(info *UpdateInfo, progressFn func(downloaded, total int64)) error {
	if info.Checksum == "" {
		return fmt.Errorf("no checksum available for %s - refusing to install unverified binary", info.AssetName)
	}

	fmt.Printf("Downloading %s...\n", info.AssetName)
	tempDir, err := config.MkTempDir("msgvault-update-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	archivePath := filepath.Join(tempDir, info.AssetName)
	downloadChecksum, err := downloadFile(info.DownloadURL, archivePath, info.Size, progressFn)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	fmt.Println("Verifying and installing...")
	if err := installFromArchiveWithChecksum(archivePath, info.Checksum, downloadChecksum); err != nil {
		return err
	}
	fmt.Println("Update complete.")
	return nil
}

// hashFile computes the SHA-256 hash of a file on disk.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// installFromArchiveTo verifies the checksum, extracts the archive, and installs
// the binary to dstPath. It handles both .zip and .tar.gz archives.
// If precomputedChecksum is non-empty, it is used instead of re-reading the file,
// avoiding redundant I/O when the caller already computed the hash (e.g. during download).
func installFromArchiveTo(archivePath, expectedChecksum, dstPath string, precomputedChecksum ...string) error {
	if expectedChecksum == "" {
		return errors.New("empty checksum - refusing to install unverified binary")
	}

	var checksum string
	if len(precomputedChecksum) > 0 && precomputedChecksum[0] != "" {
		checksum = precomputedChecksum[0]
	} else {
		var err error
		checksum, err = hashFile(archivePath)
		if err != nil {
			return fmt.Errorf("hash archive: %w", err)
		}
	}

	if !strings.EqualFold(checksum, expectedChecksum) {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, checksum)
	}

	extractDir, err := config.MkTempDir("msgvault-extract-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(extractDir) }()

	if strings.HasSuffix(archivePath, ".zip") {
		if err := extractZip(archivePath, extractDir); err != nil {
			return fmt.Errorf("extract: %w", err)
		}
	} else {
		if err := extractTarGz(archivePath, extractDir); err != nil {
			return fmt.Errorf("extract: %w", err)
		}
	}

	binaryName := "msgvault"
	if runtime.GOOS == "windows" {
		binaryName = "msgvault.exe"
	}
	srcPath := filepath.Join(extractDir, binaryName)
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return fmt.Errorf("binary %s not found in archive", binaryName)
	}

	return installBinaryTo(srcPath, dstPath)
}

// installFromArchiveWithChecksum is like InstallFromArchive but accepts a
// precomputed checksum to avoid re-reading the archive file.
func installFromArchiveWithChecksum(archivePath, expectedChecksum, precomputedChecksum string) error {
	currentExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find current executable: %w", err)
	}
	currentExe, err = filepath.EvalSymlinks(currentExe)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}
	binDir := filepath.Dir(currentExe)
	binaryName := "msgvault"
	if runtime.GOOS == "windows" {
		binaryName = "msgvault.exe"
	}
	dstPath := filepath.Join(binDir, binaryName)

	return installFromArchiveTo(archivePath, expectedChecksum, dstPath, precomputedChecksum)
}

// InstallFromArchive verifies the checksum, extracts the archive, and installs
// the binary to the current executable's location.
func InstallFromArchive(archivePath, expectedChecksum string) error {
	return installFromArchiveWithChecksum(archivePath, expectedChecksum, "")
}

// installBinaryTo performs the actual binary installation with backup/restore logic.
// This is separated from installBinary for testability.
//
// On Windows, the running executable cannot be deleted or overwritten, but it
// can be renamed. The rename-then-copy pattern works: the running process keeps
// its file handle to the renamed .old file, and the new binary is written to a
// fresh file at the original path. The .old file cannot be removed while the
// process is running, so cleanup is deferred to the next update invocation.
func installBinaryTo(srcPath, dstPath string) error {
	backupPath := dstPath + ".old"

	// Remove stale backup from a previous update. On Windows this may fail
	// if the previous binary is still running; that's fine — it will be
	// cleaned up on the next successful update.
	_ = os.Remove(backupPath)

	// Backup existing binary if it exists (rename works even on Windows
	// for the currently running executable).
	if _, err := os.Stat(dstPath); err == nil {
		if err := os.Rename(dstPath, backupPath); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}

	// Copy new binary to the now-vacant path.
	if err := copyFile(srcPath, dstPath); err != nil {
		// Attempt to restore backup on failure
		_ = os.Rename(backupPath, dstPath)
		return fmt.Errorf("install: %w", err)
	}

	if err := os.Chmod(dstPath, 0755); err != nil { //nolint:gosec // installed binary needs the executable bit
		return fmt.Errorf("chmod: %w", err)
	}

	// Clean up backup. On Windows this will fail for the running executable
	// (silently ignored); the stale .old file is removed at the top of the
	// next update.
	_ = os.Remove(backupPath)

	return nil
}

func getCacheDir() string {
	return config.DefaultHome()
}

// resolveLatestTag follows the /releases/latest 302 redirect to
// /releases/tag/<tag> and returns the tag. Using the HTML endpoint
// avoids api.github.com's 60-req/hr unauthenticated rate limit.
func resolveLatestTag(url string) (string, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", updateUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", fmt.Errorf("expected redirect from %s, got %s", url, resp.Status)
	}

	loc, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("read Location header: %w", err)
	}

	const marker = "/releases/tag/"
	idx := strings.Index(loc.Path, marker)
	if idx < 0 {
		return "", fmt.Errorf("unexpected redirect target %q", loc.String())
	}
	tag := loc.Path[idx+len(marker):]
	if tag == "" {
		return "", fmt.Errorf("empty tag in redirect target %q", loc.String())
	}
	return tag, nil
}

// fetchContentLength does a HEAD request and returns the Content-Length
// of the eventual asset (following redirects to the S3 backend).
// Returns 0 if the size can't be determined; callers degrade gracefully.
func fetchContentLength(url string) (int64, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", updateUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HEAD %s returned %s", url, resp.Status)
	}
	if resp.ContentLength < 0 {
		return 0, nil
	}
	return resp.ContentLength, nil
}

func downloadFile(url, dest string, totalSize int64, progressFn func(downloaded, total int64)) (string, error) {
	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}

	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer func() { _ = out.Close() }()

	hasher := sha256.New()
	writer := io.MultiWriter(out, hasher)

	var downloaded int64
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := writer.Write(buf[:n])
			if writeErr != nil {
				return "", writeErr
			}
			downloaded += int64(n)
			if progressFn != nil {
				progressFn(downloaded, totalSize)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func extractTarGz(archivePath, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolve dest dir: %w", err)
	}

	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	gzr, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer func() { _ = gzr.Close() }()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		target, err := sanitizeTarPath(absDestDir, header.Name)
		if err != nil {
			return fmt.Errorf("invalid tar entry %q: %w", header.Name, err)
		}

		// Skip symlinks and hardlinks
		if header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			outFile, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tr); err != nil { //nolint:gosec // extracting our own checksum-verified release archive
				_ = outFile.Close()
				return err
			}
			_ = outFile.Close()
			if err := os.Chmod(target, os.FileMode(header.Mode)); err != nil { //nolint:gosec // tar header mode from our own release archive
				return err
			}
		}
	}

	return nil
}

// sanitizeTarPath validates and sanitizes a tar entry path to prevent directory traversal.
func sanitizeTarPath(destDir, name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", errors.New("absolute path not allowed")
	}

	cleanName := filepath.Clean(name)

	if filepath.IsAbs(cleanName) {
		return "", errors.New("absolute path not allowed")
	}

	if strings.HasPrefix(cleanName, "..") || strings.Contains(cleanName, string(filepath.Separator)+"..") {
		return "", errors.New("path traversal not allowed")
	}

	target := filepath.Join(destDir, cleanName)

	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(absTarget, absDestDir+string(filepath.Separator)) && absTarget != absDestDir {
		return "", errors.New("path escapes destination directory")
	}

	return target, nil
}

func extractZip(archivePath, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolve dest dir: %w", err)
	}

	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()

	for _, f := range r.File {
		target, err := sanitizeTarPath(absDestDir, f.Name)
		if err != nil {
			return fmt.Errorf("invalid zip entry %q: %w", f.Name, err)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}

		outFile, err := os.Create(target)
		if err != nil {
			_ = rc.Close()
			return err
		}

		_, copyErr := io.Copy(outFile, rc) //nolint:gosec // extracting our own checksum-verified release archive
		closeErr := outFile.Close()
		_ = rc.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}

	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	return out.Close()
}

func fetchChecksumFromFile(url, assetName string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch checksums: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return extractChecksum(string(body), assetName), nil
}

func extractChecksum(releaseBody, assetName string) string {
	lines := strings.Split(releaseBody, "\n")
	re := regexp.MustCompile(`(?i)[a-f0-9]{64}`)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Parse as "checksum  filename" or "checksum filename" and compare filename exactly
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			fname := strings.TrimPrefix(fields[1], "*") // sha256sum -b uses *filename
			if fname == assetName {
				if match := re.FindString(fields[0]); match != "" {
					return strings.ToLower(match)
				}
			}
		}
	}
	return ""
}

func loadCache() (*cachedCheck, error) {
	cachePath := filepath.Join(getCacheDir(), cacheFileName)
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, err
	}
	var cached cachedCheck
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, err
	}
	return &cached, nil
}

// checkCache checks if a valid cached update check exists.
// Returns (info, true) if a cached result should be used (either an update or no update).
// Returns (nil, false) if no valid cache exists and a fresh check is needed.
func checkCache(currentVersion, cleanVersion string, isDevBuild bool) (*UpdateInfo, bool) {
	cached, err := loadCache()
	if err != nil {
		return nil, false
	}

	cacheWindow := cacheDuration
	if isDevBuild {
		cacheWindow = devCacheDuration
	}

	if time.Since(cached.CheckedAt) >= cacheWindow {
		return nil, false
	}

	latestVersion := strings.TrimPrefix(cached.Version, "v")

	// Dev builds always show update info (no version comparison)
	if isDevBuild {
		return &UpdateInfo{
			CurrentVersion: currentVersion,
			LatestVersion:  cached.Version,
			IsDevBuild:     true,
		}, true
	}

	// For release builds, check if there's actually an update
	if !isNewer(latestVersion, cleanVersion) {
		return nil, true // No update available, but cache is valid
	}

	return nil, false // Update available but need fresh data for download info
}

func saveCache(version string) {
	cached := cachedCheck{
		CheckedAt: time.Now(),
		Version:   version,
	}
	data, err := json.Marshal(cached)
	if err != nil {
		return
	}
	cachePath := filepath.Join(getCacheDir(), cacheFileName)
	os.MkdirAll(filepath.Dir(cachePath), 0755)      //nolint:errcheck,gosec
	fileutil.SecureWriteFile(cachePath, data, 0600) //nolint:errcheck,gosec
}

// extractBaseSemver extracts the base semver from a version string.
func extractBaseSemver(v string) string {
	v = strings.TrimPrefix(v, "v")
	if len(v) == 0 || v[0] < '0' || v[0] > '9' {
		return ""
	}
	if !strings.Contains(v, ".") {
		return ""
	}
	if idx := strings.Index(v, "-"); idx > 0 {
		v = v[:idx]
	}
	return v
}

// gitDescribePattern matches git describe format: v0.16.1-2-gabcdef or v0.16.1-2-gabcdef-dirty.
var gitDescribePattern = regexp.MustCompile(`-\d+-g[0-9a-f]+(-dirty)?$`)

// isDevBuildVersion returns true if the version is a dev build.
func isDevBuildVersion(v string) bool {
	v = strings.TrimPrefix(v, "v")
	if extractBaseSemver(v) == "" {
		return true
	}
	return gitDescribePattern.MatchString(v)
}

// isNewer returns true if v1 is newer than v2 (semver comparison).
// Prerelease versions (e.g. -rc1) are considered older than the same base version.
// Git-describe versions (e.g. 0.4.0-5-gabcdef) are treated as their base version.
func isNewer(v1, v2 string) bool {
	// Extract base semver to validate both are valid versions
	base1 := extractBaseSemver(v1)
	base2 := extractBaseSemver(v2)
	if base1 == "" || base2 == "" {
		return false
	}

	// Normalize to semver format with "v" prefix
	sv1 := normalizeSemver(v1)
	sv2 := normalizeSemver(v2)

	return semver.Compare(sv1, sv2) > 0
}

// prereleaseNumericPattern matches prerelease identifiers consisting of letters followed
// by digits (e.g., "rc10", "beta2", "alpha1") to normalize them for proper numeric comparison.
// The pattern is anchored to avoid partial matches within identifiers like "rc10a".
var prereleaseNumericPattern = regexp.MustCompile(`^([A-Za-z]+)(\d+)$`)

// normalizeSemver converts a version string to semver format for comparison.
// Git-describe versions are converted to their base version.
// Prerelease tags are normalized to use dotted format for proper numeric comparison
// (e.g., "rc10" becomes "rc.10" so that rc.10 > rc.2 numerically).
func normalizeSemver(v string) string {
	v = strings.TrimPrefix(v, "v")

	// Strip git-describe suffix (e.g., "-5-gabcdef" or "-5-gabcdef-dirty")
	if gitDescribePattern.MatchString(v) {
		v = gitDescribePattern.ReplaceAllString(v, "")
	}

	// Normalize prerelease identifiers to dotted format for numeric comparison.
	// Per semver spec, "rc10" is compared lexicographically (so rc10 < rc2).
	// By converting to "rc.10", the numeric part is compared as an integer.
	// Each dot-separated identifier is processed independently.
	if idx := strings.Index(v, "-"); idx > 0 {
		base := v[:idx]
		prerelease := v[idx+1:]
		prerelease = normalizePrereleaseIdentifiers(prerelease)
		v = base + "-" + prerelease
	}

	return "v" + v
}

// normalizePrereleaseIdentifiers processes each dot-separated prerelease identifier
// and normalizes identifiers like "rc10" to "rc.10" for proper numeric comparison.
// Identifiers with leading zeros in the numeric part are skipped to avoid creating
// invalid semver numeric identifiers.
func normalizePrereleaseIdentifiers(prerelease string) string {
	parts := strings.Split(prerelease, ".")
	var result []string
	for _, part := range parts {
		if matches := prereleaseNumericPattern.FindStringSubmatch(part); matches != nil {
			letters, digits := matches[1], matches[2]
			// Skip normalization if the numeric part has leading zeros,
			// as that would create an invalid semver numeric identifier.
			if len(digits) > 1 && digits[0] == '0' {
				result = append(result, part)
			} else {
				result = append(result, letters, digits)
			}
		} else {
			result = append(result, part)
		}
	}
	return strings.Join(result, ".")
}

// FormatSize formats bytes as a human-readable string.
func FormatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
