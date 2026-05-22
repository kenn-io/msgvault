package cmd

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/importer/mboxzip"
)

func writeZipFile(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer func() { _ = f.Close() }()

	zw := zip.NewWriter(f)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write zip entry %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
}

func writeZipFileStored(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer func() { _ = f.Close() }()

	zw := zip.NewWriter(f)
	for name, content := range entries {
		hdr := &zip.FileHeader{
			Name:   name,
			Method: zip.Store,
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("create zip entry %q: %v", name, err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatalf("write zip entry %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
}

func corruptZipFileBytes(t *testing.T, zipPath string, needle []byte) {
	t.Helper()
	b, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	if n := bytes.Count(b, needle); n != 1 {
		t.Fatalf("expected needle to appear once in zip, got %d matches", n)
	}
	idx := bytes.Index(b, needle)
	if idx == -1 {
		t.Fatalf("needle not found in zip")
	}
	// Flip one byte in the stored payload to trigger a CRC mismatch on extraction.
	b[idx] ^= 0xff
	if err := os.WriteFile(zipPath, b, 0600); err != nil {
		t.Fatalf("write corrupted zip: %v", err)
	}
}

func zeroZipCentralDirUncompressedSize(t *testing.T, zipPath string, entryName string) {
	t.Helper()

	b, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}

	// Find End of Central Directory record (EOCD). Search backwards since there's an optional comment.
	const (
		eocdLen              = 22
		maxCommentLen        = 1<<16 - 1
		eocdSig       uint32 = 0x06054b50
	)
	start := len(b) - (eocdLen + maxCommentLen)
	if start < 0 {
		start = 0
	}
	eocd := -1
	for i := len(b) - eocdLen; i >= start; i-- {
		if binary.LittleEndian.Uint32(b[i:]) == eocdSig {
			eocd = i
			break
		}
	}
	if eocd == -1 {
		t.Fatalf("eocd not found")
	}

	cdSize := int(binary.LittleEndian.Uint32(b[eocd+12:]))
	cdOff := int(binary.LittleEndian.Uint32(b[eocd+16:]))
	if cdOff < 0 || cdSize < 0 || cdOff+cdSize > len(b) {
		t.Fatalf("central directory out of bounds (off=%d size=%d len=%d)", cdOff, cdSize, len(b))
	}

	// Iterate central directory entries and zero the uncompressed size field for entryName.
	cd := b[cdOff : cdOff+cdSize]
	const cdfhSig uint32 = 0x02014b50
	off := 0
	found := false
	for off+46 <= len(cd) {
		if binary.LittleEndian.Uint32(cd[off:]) != cdfhSig {
			t.Fatalf("central directory header signature mismatch at offset %d", off)
		}
		nameLen := int(binary.LittleEndian.Uint16(cd[off+28:]))
		extraLen := int(binary.LittleEndian.Uint16(cd[off+30:]))
		commentLen := int(binary.LittleEndian.Uint16(cd[off+32:]))
		if off+46+nameLen+extraLen+commentLen > len(cd) {
			t.Fatalf("central directory entry out of bounds")
		}

		name := cd[off+46 : off+46+nameLen]
		if bytes.Equal(name, []byte(entryName)) {
			for i := 0; i < 4; i++ {
				cd[off+24+i] = 0
			}
			found = true
			break
		}
		off += 46 + nameLen + extraLen + commentLen
	}
	if !found {
		t.Fatalf("central directory entry %q not found", entryName)
	}

	if err := os.WriteFile(zipPath, b, 0600); err != nil {
		t.Fatalf("write patched zip: %v", err)
	}
}

func TestResolveMboxExport_ZipExtractsAndCaches(t *testing.T) {
	tmp := t.TempDir()

	// Resolve symlinks / 8.3 short names so path comparisons work on Windows.
	evalTmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n",
		"sent.mbx":   "From a@b Sat Jan 1 00:00:01 2024\nSubject: y\n\nBody2\n",
	})

	files1, err := mboxzip.ResolveMboxExport(zipPath, tmp, nil)
	if err != nil {
		t.Fatalf("resolveMboxExport: %v", err)
	}
	if len(files1) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files1))
	}
	if files1[0] == files1[1] {
		t.Fatalf("expected distinct extracted files, got %q", files1[0])
	}

	// Verify files exist and are in the expected extracted directory.
	for _, p := range files1 {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("stat extracted file %q: %v", p, err)
		}
		if !strings.Contains(filepath.Dir(p), filepath.Join(evalTmp, "imports", "mbox")) {
			t.Fatalf("unexpected extracted dir for %q", p)
		}
	}

	// Second run should reuse the extracted files (sentinel-based caching).
	files2, err := mboxzip.ResolveMboxExport(zipPath, tmp, nil)
	if err != nil {
		t.Fatalf("resolveMboxExport (2nd): %v", err)
	}
	if strings.Join(files1, "|") != strings.Join(files2, "|") {
		t.Fatalf("cached files mismatch:\n1=%v\n2=%v", files1, files2)
	}
}

func TestResolveMboxExport_ZipTouchDoesNotInvalidateCache(t *testing.T) {
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n",
	})

	files1, err := mboxzip.ResolveMboxExport(zipPath, tmp, nil)
	if err != nil {
		t.Fatalf("resolveMboxExport: %v", err)
	}
	if len(files1) != 1 {
		t.Fatalf("len(files) = %d, want 1", len(files1))
	}

	// Touch the zip without changing its contents; cache key should remain stable.
	touch := time.Now().Add(10 * time.Second)
	if err := os.Chtimes(zipPath, touch, touch); err != nil {
		t.Fatalf("chtimes zip: %v", err)
	}

	files2, err := mboxzip.ResolveMboxExport(zipPath, tmp, nil)
	if err != nil {
		t.Fatalf("resolveMboxExport (2nd): %v", err)
	}
	if strings.Join(files1, "|") != strings.Join(files2, "|") {
		t.Fatalf("cached files mismatch:\n1=%v\n2=%v", files1, files2)
	}
}

func TestExtractMboxFromZip_CacheValidationRejectsUnknownUncompressedSizeCRCMismatch(t *testing.T) {
	t.Setenv("MSGVAULT_ZIP_CACHE_VALIDATE_CRC32", "")

	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n",
	})
	zeroZipCentralDirUncompressedSize(t, zipPath, "inbox.mbox")

	destDir := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(destDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, ".done"), []byte("ok\n"), 0600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	wantPath := filepath.Join(destDir, "inbox.mbox")
	if err := os.WriteFile(wantPath, []byte("cached"), 0600); err != nil {
		t.Fatalf("write cached file: %v", err)
	}

	_, err := mboxzip.ValidateExtractedMboxCache(zipPath, destDir, mboxzip.ExtractLimits{
		MaxEntryBytes: mboxzip.DefaultMaxZipEntryBytes,
		MaxTotalBytes: mboxzip.DefaultMaxZipTotalBytes,
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "crc32") {
		t.Fatalf("expected crc32 error, got %v", err)
	}
}

func TestExtractMboxFromZip_CacheValidationRejectsEmptyEntrySizeMismatch(t *testing.T) {
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"empty.mbox": "",
	})

	destDir := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(destDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, ".done"), []byte("ok\n"), 0600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	wantPath := filepath.Join(destDir, "empty.mbox")
	if err := os.WriteFile(wantPath, []byte("cached"), 0600); err != nil {
		t.Fatalf("write cached file: %v", err)
	}

	_, err := mboxzip.ValidateExtractedMboxCache(zipPath, destDir, mboxzip.ExtractLimits{
		MaxEntryBytes: mboxzip.DefaultMaxZipEntryBytes,
		MaxTotalBytes: mboxzip.DefaultMaxZipTotalBytes,
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "crc32") {
		t.Fatalf("expected crc32 error, got %v", err)
	}
}

func TestExtractMboxFromZip_CacheValidationRejectsSameSizeCRCMismatch(t *testing.T) {
	t.Setenv("MSGVAULT_ZIP_CACHE_VALIDATE_CRC32", "1")

	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "hello",
	})

	destDir := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(destDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, ".done"), []byte("ok\n"), 0600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	wantPath := filepath.Join(destDir, "inbox.mbox")
	if err := os.WriteFile(wantPath, []byte("jello"), 0600); err != nil { // same size as "hello"
		t.Fatalf("write cached file: %v", err)
	}

	_, err := mboxzip.ValidateExtractedMboxCache(zipPath, destDir, mboxzip.ExtractLimits{
		MaxEntryBytes: mboxzip.DefaultMaxZipEntryBytes,
		MaxTotalBytes: mboxzip.DefaultMaxZipTotalBytes,
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "crc32") {
		t.Fatalf("expected crc32 error, got %v", err)
	}
}

func TestExtractMboxFromZip_CacheValidationSkipsCRCByDefaultWhenSizeKnown(t *testing.T) {
	t.Setenv("MSGVAULT_ZIP_CACHE_VALIDATE_CRC32", "")

	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "hello",
	})

	destDir := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(destDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, ".done"), []byte("ok\n"), 0600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	wantPath := filepath.Join(destDir, "inbox.mbox")
	if err := os.WriteFile(wantPath, []byte("jello"), 0600); err != nil { // same size as "hello"
		t.Fatalf("write cached file: %v", err)
	}

	_, err := mboxzip.ValidateExtractedMboxCache(zipPath, destDir, mboxzip.ExtractLimits{
		MaxEntryBytes: mboxzip.DefaultMaxZipEntryBytes,
		MaxTotalBytes: mboxzip.DefaultMaxZipTotalBytes,
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestExtractMboxFromZip_CacheValidationRejectsExtraFiles(t *testing.T) {
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	const inbox = "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n"
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": inbox,
	})

	destDir := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(destDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, ".done"), []byte("ok\n"), 0600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	wantPath := filepath.Join(destDir, "inbox.mbox")
	if err := os.WriteFile(wantPath, []byte(inbox), 0600); err != nil {
		t.Fatalf("write cached file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, "extra.txt"), []byte("x"), 0600); err != nil {
		t.Fatalf("write extra file: %v", err)
	}

	_, err := mboxzip.ValidateExtractedMboxCache(zipPath, destDir, mboxzip.ExtractLimits{
		MaxEntryBytes: mboxzip.DefaultMaxZipEntryBytes,
		MaxTotalBytes: mboxzip.DefaultMaxZipTotalBytes,
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("expected unexpected-file error, got %v", err)
	}
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
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, zip.ErrChecksum) {
		t.Fatalf("expected zip.ErrChecksum, got %v", err)
	}
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
	if n != 3 {
		t.Fatalf("n=%d, want 3", n)
	}
	if !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("expected ErrNoProgress, got %v", err)
	}
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
	if err != nil {
		t.Fatalf("extractMboxFromZip: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}

	b0 := filepath.Base(files[0])
	b1 := filepath.Base(files[1])
	if b0 == b1 {
		t.Fatalf("expected disambiguated output names, got %q", b0)
	}
}

func TestExtractMboxFromZip_DoesNotOverwriteOnCraftedNameCollision(t *testing.T) {
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")

	// Create an entry whose literal filename matches the disambiguated name for "b/inbox.mbox".
	cleanName := path.Clean(strings.ReplaceAll("b/inbox.mbox", "\\", "/"))
	sum := sha256.Sum256([]byte(cleanName))
	literalName := "inbox_" + hex.EncodeToString(sum[:4]) + ".mbox"

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	zw := zip.NewWriter(f)
	writeEntry := func(name, content string) {
		t.Helper()
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write zip entry %q: %v", name, err)
		}
	}
	writeEntry(literalName, "literal")
	writeEntry("a/inbox.mbox", "a")
	writeEntry("b/inbox.mbox", "b")

	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	destDir := filepath.Join(tmp, "extract")
	files, err := mboxzip.ExtractMboxFromZip(zipPath, destDir, nil)
	if err != nil {
		t.Fatalf("extractMboxFromZip: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("len(files) = %d, want 3", len(files))
	}

	seen := make(map[string]struct{})
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read extracted file %q: %v", p, err)
		}
		seen[string(b)] = struct{}{}
	}
	for _, want := range []string{"literal", "a", "b"} {
		if _, ok := seen[want]; !ok {
			t.Fatalf("missing extracted content %q; got %v", want, seen)
		}
	}
}

func TestExtractMboxFromZip_FlattensTraversalNamesSafely(t *testing.T) {
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"../evil.mbox": "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n",
	})

	destDir := filepath.Join(tmp, "extract")
	files, err := mboxzip.ExtractMboxFromZip(zipPath, destDir, nil)
	if err != nil {
		t.Fatalf("extractMboxFromZip: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1", len(files))
	}
	if filepath.Dir(files[0]) != destDir {
		t.Fatalf("expected extracted file under destDir; got %q", files[0])
	}
	if filepath.Base(files[0]) != "evil.mbox" {
		t.Fatalf("expected flattened base name evil.mbox, got %q", filepath.Base(files[0]))
	}
}

func TestExtractMboxFromZip_SanitizesWindowsInvalidFilenames(t *testing.T) {
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"Inbox:2024.mbox": "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n",
	})

	destDir := filepath.Join(tmp, "extract")
	files, err := mboxzip.ExtractMboxFromZip(zipPath, destDir, nil)
	if err != nil {
		t.Fatalf("extractMboxFromZip: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1", len(files))
	}
	if got := filepath.Base(files[0]); got != "Inbox_2024.mbox" {
		t.Fatalf("base name = %q, want %q", got, "Inbox_2024.mbox")
	}
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
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected limit error, got %v", err)
	}
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
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected limit error, got %v", err)
	}
}

func TestResolveMboxExport_Zip_ReturnsAbsolutePathsWhenImportsDirRelative(t *testing.T) {
	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "From a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n",
	})

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	importsRel, err := filepath.Rel(wd, tmp)
	if err != nil {
		t.Skipf("cannot make relative path (cross-drive on Windows): %v", err)
	}
	files, err := mboxzip.ResolveMboxExport(zipPath, importsRel, nil)
	if err != nil {
		t.Fatalf("resolveMboxExport: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1", len(files))
	}
	if !filepath.IsAbs(files[0]) {
		t.Fatalf("expected absolute extracted path, got %q", files[0])
	}
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
	if err := os.MkdirAll(realImports, 0700); err != nil {
		t.Fatalf("mkdir real imports: %v", err)
	}
	linkImports := filepath.Join(tmp, "link")
	if err := os.Symlink(realImports, linkImports); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	_, err := mboxzip.ResolveMboxExport(zipPath, linkImports, nil)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink error, got %v", err)
	}
}

func TestResolveMboxExport_RejectsNonRegularFile(t *testing.T) {
	tmp := t.TempDir()

	// Looks like a zip export but is a directory.
	exportPath := filepath.Join(tmp, "export.zip")
	if err := os.MkdirAll(exportPath, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := mboxzip.ResolveMboxExport(exportPath, tmp, nil)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("expected regular file error, got %v", err)
	}
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
	if err := os.MkdirAll(targetDir, 0700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	destDir := filepath.Join(tmp, "extract")
	if err := os.Symlink(targetDir, destDir); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	_, err := mboxzip.ExtractMboxFromZip(zipPath, destDir, nil)
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestExtractMboxFromZip_DoesNotWriteThroughPreExistingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlink support")
	}

	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "zipdata",
	})

	destDir := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(destDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	target := filepath.Join(tmp, "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	outPath := filepath.Join(destDir, "inbox.mbox")
	if err := os.Symlink(target, outPath); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	files, err := mboxzip.ExtractMboxFromZip(zipPath, destDir, nil)
	if err != nil {
		t.Fatalf("extractMboxFromZip: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1", len(files))
	}

	if b, err := os.ReadFile(target); err != nil {
		t.Fatalf("read target: %v", err)
	} else if string(b) != "keep" {
		t.Fatalf("target was modified: %q", string(b))
	}

	st, err := os.Lstat(files[0])
	if err != nil {
		t.Fatalf("lstat extracted file: %v", err)
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		t.Fatalf("expected regular extracted file, got mode %v", st.Mode())
	}
	if b, err := os.ReadFile(files[0]); err != nil {
		t.Fatalf("read extracted file: %v", err)
	} else if string(b) != "zipdata" {
		t.Fatalf("extracted contents = %q, want %q", string(b), "zipdata")
	}
}

func TestExtractMboxFromZip_CachedExtractionRejectsSymlinkedFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlink support")
	}

	tmp := t.TempDir()

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"inbox.mbox": "zipdata",
	})

	destDir := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(destDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, ".done"), []byte("ok\n"), 0600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	target := filepath.Join(tmp, "target")
	if err := os.WriteFile(target, []byte("evil"), 0600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	outPath := filepath.Join(destDir, "inbox.mbox")
	if err := os.Symlink(target, outPath); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	files, err := mboxzip.ExtractMboxFromZip(zipPath, destDir, nil)
	if err != nil {
		t.Fatalf("extractMboxFromZip: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1", len(files))
	}

	st, err := os.Lstat(files[0])
	if err != nil {
		t.Fatalf("lstat extracted file: %v", err)
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		t.Fatalf("expected regular extracted file, got mode %v", st.Mode())
	}
	if b, err := os.ReadFile(files[0]); err != nil {
		t.Fatalf("read extracted file: %v", err)
	} else if string(b) != "zipdata" {
		t.Fatalf("extracted contents = %q, want %q", string(b), "zipdata")
	}
}
