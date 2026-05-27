package cmd

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer/mboxzip"
)

func writeZipFile(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	requirepkg.NoError(t, err, "create zip")
	defer func() { _ = f.Close() }()

	zw := zip.NewWriter(f)
	for name, content := range entries {
		w, err := zw.Create(name)
		requirepkg.NoError(t, err, "create zip entry %q", name)
		_, err = w.Write([]byte(content))
		requirepkg.NoError(t, err, "write zip entry %q", name)
	}
	requirepkg.NoError(t, zw.Close(), "close zip")
}

func writeZipFileStored(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	requirepkg.NoError(t, err, "create zip")
	defer func() { _ = f.Close() }()

	zw := zip.NewWriter(f)
	for name, content := range entries {
		hdr := &zip.FileHeader{
			Name:   name,
			Method: zip.Store,
		}
		w, err := zw.CreateHeader(hdr)
		requirepkg.NoError(t, err, "create zip entry %q", name)
		_, err = w.Write(content)
		requirepkg.NoError(t, err, "write zip entry %q", name)
	}
	requirepkg.NoError(t, zw.Close(), "close zip")
}

func corruptZipFileBytes(t *testing.T, zipPath string, needle []byte) {
	t.Helper()
	b, err := os.ReadFile(zipPath)
	requirepkg.NoError(t, err, "read zip")
	n := bytes.Count(b, needle)
	requirepkg.Equal(t, 1, n, "expected needle to appear once in zip, got %d matches", n)
	idx := bytes.Index(b, needle)
	requirepkg.NotEqual(t, -1, idx, "needle not found in zip")
	// Flip one byte in the stored payload to trigger a CRC mismatch on extraction.
	b[idx] ^= 0xff
	requirepkg.NoError(t, os.WriteFile(zipPath, b, 0600), "write corrupted zip")
}

func zeroZipCentralDirUncompressedSize(t *testing.T, zipPath string, entryName string) {
	t.Helper()

	b, err := os.ReadFile(zipPath)
	requirepkg.NoError(t, err, "read zip")

	// Find End of Central Directory record (EOCD). Search backwards since there's an optional comment.
	const (
		eocdLen              = 22
		maxCommentLen        = 1<<16 - 1
		eocdSig       uint32 = 0x06054b50
	)
	start := max(len(b)-(eocdLen+maxCommentLen), 0)
	eocd := -1
	for i := len(b) - eocdLen; i >= start; i-- {
		if binary.LittleEndian.Uint32(b[i:]) == eocdSig {
			eocd = i
			break
		}
	}
	requirepkg.NotEqual(t, -1, eocd, "eocd not found")

	cdSize := int(binary.LittleEndian.Uint32(b[eocd+12:]))
	cdOff := int(binary.LittleEndian.Uint32(b[eocd+16:]))
	requirepkg.False(t, cdOff < 0 || cdSize < 0 || cdOff+cdSize > len(b),
		"central directory out of bounds (off=%d size=%d len=%d)", cdOff, cdSize, len(b))

	// Iterate central directory entries and zero the uncompressed size field for entryName.
	cd := b[cdOff : cdOff+cdSize]
	const cdfhSig uint32 = 0x02014b50
	off := 0
	found := false
	for off+46 <= len(cd) {
		requirepkg.Equal(t, cdfhSig, binary.LittleEndian.Uint32(cd[off:]),
			"central directory header signature mismatch at offset %d", off)
		nameLen := int(binary.LittleEndian.Uint16(cd[off+28:]))
		extraLen := int(binary.LittleEndian.Uint16(cd[off+30:]))
		commentLen := int(binary.LittleEndian.Uint16(cd[off+32:]))
		requirepkg.LessOrEqual(t, off+46+nameLen+extraLen+commentLen, len(cd),
			"central directory entry out of bounds")

		name := cd[off+46 : off+46+nameLen]
		if bytes.Equal(name, []byte(entryName)) {
			for i := range 4 {
				cd[off+24+i] = 0
			}
			found = true
			break
		}
		off += 46 + nameLen + extraLen + commentLen
	}
	requirepkg.True(t, found, "central directory entry %q not found", entryName)

	requirepkg.NoError(t, os.WriteFile(zipPath, b, 0600), "write patched zip")
}

func TestResolveMboxExport_ZipExtractsAndCaches(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	// Resolve symlinks / 8.3 short names so path comparisons work on Windows.
	evalTmp, err := filepath.EvalSymlinks(tmp)
	require.NoError(err, "eval symlinks")

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n",
		"sent.mbx":   "From a@b Sat Jan 1 00:00:01 2024\nSubject: y\n\nBody2\n",
	})

	files1, err := mboxzip.ResolveMboxExport(zipPath, tmp, nil)
	require.NoError(err, "resolveMboxExport")
	require.Len(files1, 2)
	require.NotEqual(files1[0], files1[1], "expected distinct extracted files, got %q", files1[0])

	// Verify files exist and are in the expected extracted directory.
	for _, p := range files1 {
		_, err := os.Stat(p)
		require.NoError(err, "stat extracted file %q", p)
		require.Contains(filepath.Dir(p), filepath.Join(evalTmp, "imports", "mbox"),
			"unexpected extracted dir for %q", p)
	}

	// Second run should reuse the extracted files (sentinel-based caching).
	files2, err := mboxzip.ResolveMboxExport(zipPath, tmp, nil)
	require.NoError(err, "resolveMboxExport (2nd)")
	require.Equal(strings.Join(files1, "|"), strings.Join(files2, "|"),
		"cached files mismatch:\n1=%v\n2=%v", files1, files2)
}

func TestResolveMboxExport_ZipTouchDoesNotInvalidateCache(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n",
	})

	files1, err := mboxzip.ResolveMboxExport(zipPath, tmp, nil)
	require.NoError(err, "resolveMboxExport")
	require.Len(files1, 1)

	// Touch the zip without changing its contents; cache key should remain stable.
	touch := time.Now().Add(10 * time.Second)
	require.NoError(os.Chtimes(zipPath, touch, touch), "chtimes zip")

	files2, err := mboxzip.ResolveMboxExport(zipPath, tmp, nil)
	require.NoError(err, "resolveMboxExport (2nd)")
	require.Equal(strings.Join(files1, "|"), strings.Join(files2, "|"),
		"cached files mismatch:\n1=%v\n2=%v", files1, files2)
}

func TestExtractMboxFromZip_CacheValidationRejectsUnknownUncompressedSizeCRCMismatch(t *testing.T) {
	require := requirepkg.New(t)
	t.Setenv("MSGVAULT_ZIP_CACHE_VALIDATE_CRC32", "")

	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n",
	})
	zeroZipCentralDirUncompressedSize(t, zipPath, "inbox.mbox")

	destDir := filepath.Join(tmp, "extract")
	require.NoError(os.MkdirAll(destDir, 0700), "mkdir")
	require.NoError(os.WriteFile(filepath.Join(destDir, ".done"), []byte("ok\n"), 0600), "write sentinel")
	wantPath := filepath.Join(destDir, "inbox.mbox")
	require.NoError(os.WriteFile(wantPath, []byte("cached"), 0600), "write cached file")

	_, err := mboxzip.ValidateExtractedMboxCache(zipPath, destDir, mboxzip.ExtractLimits{
		MaxEntryBytes: mboxzip.DefaultMaxZipEntryBytes,
		MaxTotalBytes: mboxzip.DefaultMaxZipTotalBytes,
	})
	require.Error(err)
	assertpkg.ErrorContains(t, err, "crc32")
}

func TestExtractMboxFromZip_CacheValidationRejectsEmptyEntrySizeMismatch(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"empty.mbox": "",
	})

	destDir := filepath.Join(tmp, "extract")
	require.NoError(os.MkdirAll(destDir, 0700), "mkdir")
	require.NoError(os.WriteFile(filepath.Join(destDir, ".done"), []byte("ok\n"), 0600), "write sentinel")
	wantPath := filepath.Join(destDir, "empty.mbox")
	require.NoError(os.WriteFile(wantPath, []byte("cached"), 0600), "write cached file")

	_, err := mboxzip.ValidateExtractedMboxCache(zipPath, destDir, mboxzip.ExtractLimits{
		MaxEntryBytes: mboxzip.DefaultMaxZipEntryBytes,
		MaxTotalBytes: mboxzip.DefaultMaxZipTotalBytes,
	})
	require.Error(err)
	assertpkg.ErrorContains(t, err, "crc32")
}

func TestExtractMboxFromZip_CacheValidationRejectsSameSizeCRCMismatch(t *testing.T) {
	require := requirepkg.New(t)
	t.Setenv("MSGVAULT_ZIP_CACHE_VALIDATE_CRC32", "1")

	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "hello",
	})

	destDir := filepath.Join(tmp, "extract")
	require.NoError(os.MkdirAll(destDir, 0700), "mkdir")
	require.NoError(os.WriteFile(filepath.Join(destDir, ".done"), []byte("ok\n"), 0600), "write sentinel")
	wantPath := filepath.Join(destDir, "inbox.mbox")
	require.NoError(os.WriteFile(wantPath, []byte("jello"), 0600), "write cached file") // same size as "hello"

	_, err := mboxzip.ValidateExtractedMboxCache(zipPath, destDir, mboxzip.ExtractLimits{
		MaxEntryBytes: mboxzip.DefaultMaxZipEntryBytes,
		MaxTotalBytes: mboxzip.DefaultMaxZipTotalBytes,
	})
	require.Error(err)
	assertpkg.ErrorContains(t, err, "crc32")
}

func TestExtractMboxFromZip_CacheValidationSkipsCRCByDefaultWhenSizeKnown(t *testing.T) {
	require := requirepkg.New(t)
	t.Setenv("MSGVAULT_ZIP_CACHE_VALIDATE_CRC32", "")

	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "hello",
	})

	destDir := filepath.Join(tmp, "extract")
	require.NoError(os.MkdirAll(destDir, 0700), "mkdir")
	require.NoError(os.WriteFile(filepath.Join(destDir, ".done"), []byte("ok\n"), 0600), "write sentinel")
	wantPath := filepath.Join(destDir, "inbox.mbox")
	require.NoError(os.WriteFile(wantPath, []byte("jello"), 0600), "write cached file") // same size as "hello"

	_, err := mboxzip.ValidateExtractedMboxCache(zipPath, destDir, mboxzip.ExtractLimits{
		MaxEntryBytes: mboxzip.DefaultMaxZipEntryBytes,
		MaxTotalBytes: mboxzip.DefaultMaxZipTotalBytes,
	})
	require.NoError(err, "expected success")
}

func TestExtractMboxFromZip_CacheValidationRejectsExtraFiles(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	const inbox = "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n"
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": inbox,
	})

	destDir := filepath.Join(tmp, "extract")
	require.NoError(os.MkdirAll(destDir, 0700), "mkdir")
	require.NoError(os.WriteFile(filepath.Join(destDir, ".done"), []byte("ok\n"), 0600), "write sentinel")
	wantPath := filepath.Join(destDir, "inbox.mbox")
	require.NoError(os.WriteFile(wantPath, []byte(inbox), 0600), "write cached file")
	require.NoError(os.WriteFile(filepath.Join(destDir, "extra.txt"), []byte("x"), 0600), "write extra file")

	_, err := mboxzip.ValidateExtractedMboxCache(zipPath, destDir, mboxzip.ExtractLimits{
		MaxEntryBytes: mboxzip.DefaultMaxZipEntryBytes,
		MaxTotalBytes: mboxzip.DefaultMaxZipTotalBytes,
	})
	require.Error(err)
	assertpkg.ErrorContains(t, err, "unexpected")
}

func TestExtractMboxFromZip_RejectsZipChecksumError(t *testing.T) {
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	content := []byte("From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n")
	writeZipFileStored(t, zipPath, map[string][]byte{
		"inbox.mbox": content,
	})
	corruptZipFileBytes(t, zipPath, content)

	destDir := filepath.Join(tmp, "extract")
	_, err := mboxzip.ExtractMboxFromZip(zipPath, destDir, nil)
	requirepkg.Error(t, err)
	requirepkg.ErrorIs(t, err, zip.ErrChecksum)
}

type noProgressReader struct {
	b   []byte
	off int
}

func (r *noProgressReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, nil
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}

func TestCopyWithLimit_NoProgressAfterLimit_ReturnsErrNoProgress(t *testing.T) {
	var dst bytes.Buffer
	src := &noProgressReader{b: []byte("abc")}

	n, err := mboxzip.CopyWithLimit(&dst, src, 3)
	requirepkg.Equal(t, int64(3), n)
	requirepkg.ErrorIs(t, err, io.ErrNoProgress)
}

func TestExtractMboxFromZip_DisambiguatesCollidingBaseNames(t *testing.T) {
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"a/inbox.mbox": "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n",
		"b/inbox.mbox": "From a@b Sat Jan 1 00:00:01 2024\nSubject: y\n\nBody2\n",
	})

	destDir := filepath.Join(tmp, "extract")
	files, err := mboxzip.ExtractMboxFromZip(zipPath, destDir, nil)
	requirepkg.NoError(t, err, "extractMboxFromZip")
	requirepkg.Len(t, files, 2)

	b0 := filepath.Base(files[0])
	b1 := filepath.Base(files[1])
	assertpkg.NotEqual(t, b0, b1, "expected disambiguated output names, got %q", b0)
}

func TestExtractMboxFromZip_DoesNotOverwriteOnCraftedNameCollision(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")

	// Create an entry whose literal filename matches the disambiguated name for "b/inbox.mbox".
	cleanName := path.Clean(strings.ReplaceAll("b/inbox.mbox", "\\", "/"))
	sum := sha256.Sum256([]byte(cleanName))
	literalName := "inbox_" + hex.EncodeToString(sum[:4]) + ".mbox"

	f, err := os.Create(zipPath)
	require.NoError(err, "create zip")
	t.Cleanup(func() { _ = f.Close() })

	zw := zip.NewWriter(f)
	writeEntry := func(name, content string) {
		t.Helper()
		w, err := zw.Create(name)
		require.NoError(err, "create zip entry %q", name)
		_, err = w.Write([]byte(content))
		require.NoError(err, "write zip entry %q", name)
	}
	writeEntry(literalName, "literal")
	writeEntry("a/inbox.mbox", "a")
	writeEntry("b/inbox.mbox", "b")

	require.NoError(zw.Close(), "close zip")
	require.NoError(f.Close(), "close file")

	destDir := filepath.Join(tmp, "extract")
	files, err := mboxzip.ExtractMboxFromZip(zipPath, destDir, nil)
	require.NoError(err, "extractMboxFromZip")
	require.Len(files, 3)

	seen := make(map[string]struct{})
	for _, p := range files {
		b, err := os.ReadFile(p)
		require.NoError(err, "read extracted file %q", p)
		seen[string(b)] = struct{}{}
	}
	for _, want := range []string{"literal", "a", "b"} {
		_, ok := seen[want]
		assertpkg.True(t, ok, "missing extracted content %q; got %v", want, seen)
	}
}

func TestExtractMboxFromZip_FlattensTraversalNamesSafely(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"../evil.mbox": "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n",
	})

	destDir := filepath.Join(tmp, "extract")
	files, err := mboxzip.ExtractMboxFromZip(zipPath, destDir, nil)
	require.NoError(err, "extractMboxFromZip")
	require.Len(files, 1)
	assert.Equal(destDir, filepath.Dir(files[0]), "expected extracted file under destDir")
	assert.Equal("evil.mbox", filepath.Base(files[0]), "expected flattened base name")
}

func TestExtractMboxFromZip_SanitizesWindowsInvalidFilenames(t *testing.T) {
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"Inbox:2024.mbox": "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n",
	})

	destDir := filepath.Join(tmp, "extract")
	files, err := mboxzip.ExtractMboxFromZip(zipPath, destDir, nil)
	requirepkg.NoError(t, err, "extractMboxFromZip")
	requirepkg.Len(t, files, 1)
	assertpkg.Equal(t, "Inbox_2024.mbox", filepath.Base(files[0]))
}

func TestExtractMboxFromZip_EnforcesEntrySizeLimit(t *testing.T) {
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"big.mbox": strings.Repeat("a", 11),
	})

	destDir := filepath.Join(tmp, "extract")
	_, err := mboxzip.ExtractMboxFromZipWithLimits(zipPath, destDir, mboxzip.ExtractLimits{
		MaxEntryBytes: 10,
		MaxTotalBytes: 0,
	}, nil)
	requirepkg.Error(t, err)
	assertpkg.ErrorContains(t, err, "limit")
}

func TestExtractMboxFromZip_EnforcesTotalSizeLimit(t *testing.T) {
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"a.mbox": strings.Repeat("a", 6),
		"b.mbox": strings.Repeat("b", 6),
	})

	destDir := filepath.Join(tmp, "extract")
	_, err := mboxzip.ExtractMboxFromZipWithLimits(zipPath, destDir, mboxzip.ExtractLimits{
		MaxEntryBytes: 100,
		MaxTotalBytes: 10,
	}, nil)
	requirepkg.Error(t, err)
	assertpkg.ErrorContains(t, err, "limit")
}

func TestResolveMboxExport_Zip_ReturnsAbsolutePathsWhenImportsDirRelative(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n",
	})

	wd, err := os.Getwd()
	require.NoError(err, "getwd")
	importsRel, err := filepath.Rel(wd, tmp)
	if err != nil {
		t.Skipf("cannot make relative path (cross-drive on Windows): %v", err)
	}
	files, err := mboxzip.ResolveMboxExport(zipPath, importsRel, nil)
	require.NoError(err, "resolveMboxExport")
	require.Len(files, 1)
	assertpkg.True(t, filepath.IsAbs(files[0]), "expected absolute extracted path, got %q", files[0])
}

func TestResolveMboxExport_Zip_RejectsSymlinkedImportsDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlink support")
	}

	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n",
	})

	realImports := filepath.Join(tmp, "real")
	requirepkg.NoError(t, os.MkdirAll(realImports, 0700), "mkdir real imports")
	linkImports := filepath.Join(tmp, "link")
	if err := os.Symlink(realImports, linkImports); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	_, err := mboxzip.ResolveMboxExport(zipPath, linkImports, nil)
	requirepkg.Error(t, err)
	assertpkg.ErrorContains(t, err, "symlink")
}

func TestResolveMboxExport_RejectsNonRegularFile(t *testing.T) {
	tmp := t.TempDir()

	// Looks like a zip export but is a directory.
	exportPath := filepath.Join(tmp, "export.zip")
	requirepkg.NoError(t, os.MkdirAll(exportPath, 0700), "mkdir")

	_, err := mboxzip.ResolveMboxExport(exportPath, tmp, nil)
	requirepkg.Error(t, err)
	assertpkg.ErrorContains(t, err, "not a regular file")
}

func TestExtractMboxFromZip_RejectsSymlinkExtractDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlink support")
	}

	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n",
	})

	targetDir := filepath.Join(tmp, "target")
	requirepkg.NoError(t, os.MkdirAll(targetDir, 0700), "mkdir target")
	destDir := filepath.Join(tmp, "extract")
	if err := os.Symlink(targetDir, destDir); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	_, err := mboxzip.ExtractMboxFromZip(zipPath, destDir, nil)
	requirepkg.Error(t, err)
}

func TestExtractMboxFromZip_DoesNotWriteThroughPreExistingSymlink(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("requires symlink support")
	}

	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "zipdata",
	})

	destDir := filepath.Join(tmp, "extract")
	require.NoError(os.MkdirAll(destDir, 0700), "mkdir")

	target := filepath.Join(tmp, "target")
	require.NoError(os.WriteFile(target, []byte("keep"), 0600), "write target")
	outPath := filepath.Join(destDir, "inbox.mbox")
	if err := os.Symlink(target, outPath); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	files, err := mboxzip.ExtractMboxFromZip(zipPath, destDir, nil)
	require.NoError(err, "extractMboxFromZip")
	require.Len(files, 1)

	b, err := os.ReadFile(target)
	require.NoError(err, "read target")
	assert.Equal("keep", string(b), "target was modified")

	st, err := os.Lstat(files[0])
	require.NoError(err, "lstat extracted file")
	assert.True(st.Mode()&os.ModeSymlink == 0 && st.Mode().IsRegular(),
		"expected regular extracted file, got mode %v", st.Mode())
	b, err = os.ReadFile(files[0])
	require.NoError(err, "read extracted file")
	assert.Equal("zipdata", string(b), "extracted contents")
}

func TestExtractMboxFromZip_CachedExtractionRejectsSymlinkedFiles(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("requires symlink support")
	}

	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "zipdata",
	})

	destDir := filepath.Join(tmp, "extract")
	require.NoError(os.MkdirAll(destDir, 0700), "mkdir")
	require.NoError(os.WriteFile(filepath.Join(destDir, ".done"), []byte("ok\n"), 0600), "write sentinel")

	target := filepath.Join(tmp, "target")
	require.NoError(os.WriteFile(target, []byte("evil"), 0600), "write target")
	outPath := filepath.Join(destDir, "inbox.mbox")
	if err := os.Symlink(target, outPath); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	files, err := mboxzip.ExtractMboxFromZip(zipPath, destDir, nil)
	require.NoError(err, "extractMboxFromZip")
	require.Len(files, 1)

	st, err := os.Lstat(files[0])
	require.NoError(err, "lstat extracted file")
	assert.True(st.Mode()&os.ModeSymlink == 0 && st.Mode().IsRegular(),
		"expected regular extracted file, got mode %v", st.Mode())
	b, err := os.ReadFile(files[0])
	require.NoError(err, "read extracted file")
	assert.Equal("zipdata", string(b), "extracted contents")
}
