package mistral

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectFormatRecognizesBoundedDocumentFamilies(t *testing.T) {
	docx := documentZIP(t, map[string]string{
		ooxmlContentTypesName: docxContentTypes("application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"), "word/document.xml": "<document/>",
	})
	epub := documentZIP(t, map[string]string{
		"mimetype": "application/epub+zip", "META-INF/container.xml": "<container/>",
	})
	compound := compoundDocument(t, "WordDocument")

	tests := []struct {
		name      string
		content   []byte
		mediaType string
		wantID    string
	}{
		{name: "PDF", content: []byte("%PDF-1.7\nsynthetic"), mediaType: "application/pdf", wantID: "pdf"},
		{name: "DOCX", content: docx, mediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", wantID: "docx"},
		{name: "EPUB", content: epub, mediaType: "application/epub+zip", wantID: "epub"},
		{name: "legacy DOC", content: compound, mediaType: "application/msword", wantID: "doc"},
		{name: "CSV", content: []byte("name,value\nalpha,42\n"), mediaType: "text/csv", wantID: "csv"},
		{name: "JSON", content: []byte(`{"alpha":42}`), mediaType: "application/json", wantID: "json"},
		{name: "JSONL", content: []byte("{\"alpha\":1}\n{\"alpha\":2}\n"), mediaType: "application/x-ndjson", wantID: "jsonl"},
		{name: "XML", content: []byte(`<root><value>42</value></root>`), mediaType: "application/xml", wantID: "xml"},
		{name: "YAML", content: []byte("---\nalpha: 42\n"), mediaType: "application/yaml", wantID: "yaml"},
		{name: "LaTeX", content: []byte(`\documentclass{article}\begin{document}x\end{document}`), mediaType: "application/x-tex", wantID: "latex"},
		{name: "EML", content: []byte("From: sender@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\nSubject: Synthetic\r\n\r\nBody"), mediaType: "message/rfc822", wantID: "eml"},
		{name: "Go", content: []byte("package synthetic\n"), mediaType: "text/x-go", wantID: "go"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			format, err := DetectFormat(bytes.NewReader(test.content), int64(len(test.content)), test.mediaType)
			require.NoError(t, err)
			assert.Equal(t, test.wantID, format.ID)
		})
	}
}

func TestDetectFormatRejectsMismatchUnsafeZIPAndAmbiguousCompound(t *testing.T) {
	require := require.New(t)
	pdf := []byte("%PDF-1.7\nsynthetic")
	_, err := DetectFormat(bytes.NewReader(pdf), int64(len(pdf)), "text/plain")
	require.ErrorContains(err, "not declared")

	unsafe := documentZIP(t, map[string]string{
		ooxmlContentTypesName: docxContentTypes("application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"), "word/document.xml": "<document/>", "../escape": "x",
	})
	_, err = DetectFormat(bytes.NewReader(unsafe), int64(len(unsafe)), "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	require.ErrorContains(err, "traversing")

	compound := compoundDocument(t, "WordDocument", "Workbook")
	_, err = DetectFormat(bytes.NewReader(compound), int64(len(compound)), "application/msword")
	require.ErrorContains(err, "ambiguous")

	embedded := compoundDocumentWithEmbeddedWorkbook(t)
	format, err := DetectFormat(bytes.NewReader(embedded), int64(len(embedded)), "application/msword")
	require.NoError(err)
	assert.Equal(t, "doc", format.ID)

	rootStorage := compoundDocument(t, "WordDocument", "Workbook")
	rootStorage[1024+256+66] = 1
	format, err = DetectFormat(bytes.NewReader(rootStorage), int64(len(rootStorage)), "application/msword")
	require.NoError(err)
	assert.Equal(t, "doc", format.ID)

	invalidJSON := []byte(`{"unterminated":`)
	_, err = DetectFormat(bytes.NewReader(invalidJSON), int64(len(invalidJSON)), "application/json")
	require.ErrorContains(err, "invalid")

	macroEnabled := documentZIP(t, map[string]string{
		ooxmlContentTypesName: docxContentTypes("application/vnd.ms-word.document.macroEnabled.main+xml"),
		"word/document.xml":   "<document/>",
	})
	_, err = DetectFormat(bytes.NewReader(macroEnabled), int64(len(macroEnabled)), "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	require.ErrorContains(err, "not a supported document format")

	wrongNamespace := documentZIP(t, map[string]string{
		ooxmlContentTypesName: `<Types xmlns="urn:unrelated"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"word/document.xml":   "<document/>",
	})
	_, err = DetectFormat(bytes.NewReader(wrongNamespace), int64(len(wrongNamespace)), "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	require.ErrorContains(err, "not a supported document format")

	trailingMalformed := documentZIP(t, map[string]string{
		ooxmlContentTypesName: docxContentTypes("application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml") + "<broken>",
		"word/document.xml":   "<document/>",
	})
	_, err = DetectFormat(bytes.NewReader(trailingMalformed), int64(len(trailingMalformed)), "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	require.ErrorContains(err, "not a supported document format")

	for _, invalidXML := range []string{"", "<first/><second/>", "outside<root/>"} {
		_, err = DetectFormat(bytes.NewReader([]byte(invalidXML)), int64(len(invalidXML)), "application/xml")
		require.Error(err)
	}
}

func TestValidateZIPEndRecordRejectsUnboundedDirectory(t *testing.T) {
	archive := documentZIP(t, map[string]string{
		ooxmlContentTypesName: docxContentTypes("application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"), "word/document.xml": "<document/>",
	})
	offset := bytes.LastIndex(archive, []byte{'P', 'K', 0x05, 0x06})
	require.GreaterOrEqual(t, offset, 0)
	binary.LittleEndian.PutUint16(archive[offset+8:offset+10], maxZIPEntries+1)
	binary.LittleEndian.PutUint16(archive[offset+10:offset+12], maxZIPEntries+1)
	_, err := DetectFormat(bytes.NewReader(archive), int64(len(archive)), "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	require.ErrorContains(t, err, "central directory")
}

func TestSpoolVerifiedSourceCreatesPrivateDetectedFileAndCleansIt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := []byte("%PDF-1.7\nsynthetic")
	digest := sha256.Sum256(content)
	dir := filepath.Join(t.TempDir(), "private")
	require.NoError(os.Mkdir(dir, 0o700))
	source := &observedReadCloser{Reader: bytes.NewReader(content)}
	document, cleanup, err := SpoolVerifiedSource(t.Context(), source, SpoolOptions{
		Directory: dir, MediaType: "application/pdf", ExpectedSize: int64(len(content)),
		ExpectedSHA256: hex.EncodeToString(digest[:]), MaxBytes: 1024,
		MaxSpoolBytes: 2048, MinFreeBytes: 1,
	})
	require.NoError(err)
	assert.True(source.closed)
	assert.Equal("application/pdf", document.MediaType)
	info, err := os.Lstat(document.Path)
	require.NoError(err)
	assert.True(info.Mode().IsRegular())
	if runtime.GOOS != "windows" {
		assert.Equal(os.FileMode(0o600), info.Mode().Perm())
	}
	require.NoError(cleanup())
	assert.NoFileExists(document.Path)
	require.NoError(cleanup())
}

func TestSpoolVerifiedSourceFailsClosedAndRemovesPartialFiles(t *testing.T) {
	content := []byte("%PDF-1.7\nsynthetic")
	digest := sha256.Sum256(content)
	dir := filepath.Join(t.TempDir(), "private")
	require.NoError(t, os.Mkdir(dir, 0o700))

	tests := []struct {
		name      string
		source    *observedReadCloser
		size      int64
		hash      string
		maxBytes  int64
		mediaType string
		wantError string
	}{
		{name: "hash", source: &observedReadCloser{Reader: bytes.NewReader(content)}, size: int64(len(content)), hash: stringsOfZero(64), maxBytes: 1024, mediaType: "application/pdf", wantError: "hash mismatch"},
		{name: "size", source: &observedReadCloser{Reader: bytes.NewReader(content)}, size: int64(len(content) + 1), hash: hex.EncodeToString(digest[:]), maxBytes: 1024, mediaType: "application/pdf", wantError: "size mismatch"},
		{name: "limit", source: &observedReadCloser{Reader: bytes.NewReader(content)}, size: int64(len(content)), hash: hex.EncodeToString(digest[:]), maxBytes: 4, mediaType: "application/pdf", wantError: "invalid bounds"},
		{name: "close", source: &observedReadCloser{Reader: bytes.NewReader(content), closeErr: errors.New("synthetic close")}, size: int64(len(content)), hash: hex.EncodeToString(digest[:]), maxBytes: 1024, mediaType: "application/pdf", wantError: "close verified"},
		{name: "type", source: &observedReadCloser{Reader: bytes.NewReader(content)}, size: int64(len(content)), hash: hex.EncodeToString(digest[:]), maxBytes: 1024, mediaType: "text/plain", wantError: "not declared"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			_, _, err := SpoolVerifiedSource(t.Context(), test.source, SpoolOptions{
				Directory: dir, MediaType: test.mediaType, ExpectedSize: test.size,
				ExpectedSHA256: test.hash, MaxBytes: test.maxBytes,
				MaxSpoolBytes: 2048, MinFreeBytes: 1,
			})
			require.ErrorContains(err, test.wantError)
			assert.True(test.source.closed)
			entries, readErr := os.ReadDir(dir)
			require.NoError(readErr)
			assert.Empty(entries)
		})
	}
}

func TestSpoolVerifiedSourceSerializesQuotaReservations(t *testing.T) {
	content := []byte("%PDF-1.7\nserialized")
	digest := sha256.Sum256(content)
	dir := filepath.Join(t.TempDir(), "private")
	require.NoError(t, os.Mkdir(dir, 0o700))
	options := SpoolOptions{
		Directory: dir, MediaType: "application/pdf", ExpectedSize: int64(len(content)),
		ExpectedSHA256: hex.EncodeToString(digest[:]), MaxBytes: int64(len(content)),
		MaxSpoolBytes: int64(len(content)), MinFreeBytes: 1,
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	firstResult := make(chan spoolCallResult, 1)
	go func() {
		document, cleanup, err := SpoolVerifiedSource(t.Context(), &gatedReadCloser{
			Reader: bytes.NewReader(content), entered: entered, release: release,
		}, options)
		firstResult <- spoolCallResult{document: document, cleanup: cleanup, err: err}
	}()
	<-entered
	secondResult := make(chan spoolCallResult, 1)
	go func() {
		document, cleanup, err := SpoolVerifiedSource(
			t.Context(), io.NopCloser(bytes.NewReader(content)), options,
		)
		secondResult <- spoolCallResult{document: document, cleanup: cleanup, err: err}
	}()
	select {
	case result := <-secondResult:
		require.Failf(t, "reservation was not serialized", "second spool returned early: %v", result.err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	first := <-firstResult
	require.NoError(t, first.err)
	require.NotNil(t, first.cleanup)
	second := <-secondResult
	require.ErrorIs(t, second.err, ErrSpoolCapacity)
	require.NoError(t, first.cleanup())
}

type spoolCallResult struct {
	document Document
	cleanup  func() error
	err      error
}

type gatedReadCloser struct {
	*bytes.Reader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *gatedReadCloser) Read(buffer []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	n, err := r.Reader.Read(buffer)
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("read gated spool source: %w", err)
	}
	return n, nil
}

func (*gatedReadCloser) Close() error { return nil }

func TestSpoolQuotaFreeSpaceAndStaleScavenging(t *testing.T) {
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "private")
	require.NoError(os.Mkdir(dir, 0o700))
	stale := filepath.Join(dir, spoolFilenamePrefix+"stale")
	require.NoError(os.WriteFile(stale, []byte("stale"), 0o600))
	old := time.Now().UTC().Add(-3 * time.Hour)
	require.NoError(os.Chtimes(stale, old, old))
	live := filepath.Join(dir, spoolFilenamePrefix+"live")
	require.NoError(os.WriteFile(live, []byte("live"), 0o600))
	removed, err := ScavengeSpoolDirectory(dir, time.Now().UTC().Add(-2*time.Hour))
	require.NoError(err)
	assert.Equal(t, 1, removed)
	assert.NoFileExists(t, stale)
	assert.FileExists(t, live)

	content := []byte("%PDF-1.7\nsynthetic")
	digest := sha256.Sum256(content)
	options := SpoolOptions{
		Directory: dir, MediaType: "application/pdf", ExpectedSize: int64(len(content)),
		ExpectedSHA256: hex.EncodeToString(digest[:]), MaxBytes: int64(len(content)),
		MaxSpoolBytes: int64(len(content) + len("live") - 1), MinFreeBytes: 1,
	}
	_, _, err = SpoolVerifiedSource(
		t.Context(), &observedReadCloser{Reader: bytes.NewReader(content)}, options,
	)
	require.ErrorContains(err, "quota")
	options.MaxSpoolBytes = 2048
	options.MinFreeBytes = math.MaxInt64
	_, _, err = SpoolVerifiedSource(
		t.Context(), &observedReadCloser{Reader: bytes.NewReader(content)}, options,
	)
	require.ErrorContains(err, "free-space reserve")
}

func documentZIP(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, value := range entries {
		entry, err := writer.Create(name)
		require.NoError(t, err)
		_, err = io.WriteString(entry, value)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return output.Bytes()
}

func docxContentTypes(mainContentType string) string {
	return `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Override PartName="/word/document.xml" ContentType="` + mainContentType + `"/>` +
		`</Types>`
}

func compoundDocument(t *testing.T, streamNames ...string) []byte {
	t.Helper()
	const (
		freeSector = uint32(0xffffffff)
		endOfChain = uint32(0xfffffffe)
		fatSector  = uint32(0xfffffffd)
	)
	content := make([]byte, 3*512)
	header := content[:512]
	copy(header, compoundFileMagic)
	binary.LittleEndian.PutUint16(header[26:28], 3)
	binary.LittleEndian.PutUint16(header[28:30], 0xfffe)
	binary.LittleEndian.PutUint16(header[30:32], 9)
	binary.LittleEndian.PutUint16(header[32:34], 6)
	binary.LittleEndian.PutUint32(header[44:48], 1)
	binary.LittleEndian.PutUint32(header[48:52], 1)
	binary.LittleEndian.PutUint32(header[56:60], 4096)
	binary.LittleEndian.PutUint32(header[60:64], endOfChain)
	binary.LittleEndian.PutUint32(header[68:72], endOfChain)
	for offset := 76; offset < 512; offset += 4 {
		binary.LittleEndian.PutUint32(header[offset:offset+4], freeSector)
	}
	binary.LittleEndian.PutUint32(header[76:80], 0)

	fat := content[512:1024]
	for offset := 0; offset < len(fat); offset += 4 {
		binary.LittleEndian.PutUint32(fat[offset:offset+4], freeSector)
	}
	binary.LittleEndian.PutUint32(fat[0:4], fatSector)
	binary.LittleEndian.PutUint32(fat[4:8], endOfChain)

	directory := content[1024:]
	writeCompoundDirectoryEntry(t, directory[:128], "Root Entry", 5)
	if len(streamNames) > 0 {
		binary.LittleEndian.PutUint32(directory[76:80], 1)
	}
	for i, name := range streamNames {
		offset := (i + 1) * 128
		require.LessOrEqual(t, offset+128, len(directory))
		writeCompoundDirectoryEntry(t, directory[offset:offset+128], name, 2)
		if i+1 < len(streamNames) {
			binary.LittleEndian.PutUint32(directory[offset+72:offset+76], uint32(i+2))
		}
	}
	return content
}

func compoundDocumentWithEmbeddedWorkbook(t *testing.T) []byte {
	t.Helper()
	content := compoundDocument(t, "WordDocument", "ObjectPool", "Workbook")
	directory := content[1024:]
	// Root tree contains WordDocument and ObjectPool only.
	binary.LittleEndian.PutUint32(directory[128+72:128+76], 2)
	binary.LittleEndian.PutUint32(directory[256+72:256+76], compoundNoStream)
	// Workbook is a child of ObjectPool, not a root-level stream.
	directory[256+66] = 1
	binary.LittleEndian.PutUint32(directory[256+76:256+80], 3)
	return content
}

func writeCompoundDirectoryEntry(t *testing.T, entry []byte, name string, entryType byte) {
	t.Helper()
	encoded := make([]byte, 0, (len(name)+1)*2)
	for _, character := range name {
		require.Less(t, character, rune(0x10000))
		encoded = binary.LittleEndian.AppendUint16(encoded, uint16(character))
	}
	encoded = binary.LittleEndian.AppendUint16(encoded, 0)
	require.LessOrEqual(t, len(encoded), 64)
	copy(entry, encoded)
	binary.LittleEndian.PutUint16(entry[64:66], uint16(len(encoded)))
	entry[66] = entryType
	binary.LittleEndian.PutUint32(entry[68:72], compoundNoStream)
	binary.LittleEndian.PutUint32(entry[72:76], compoundNoStream)
	binary.LittleEndian.PutUint32(entry[76:80], compoundNoStream)
}

type observedReadCloser struct {
	io.Reader

	closed   bool
	closeErr error
}

func (r *observedReadCloser) Close() error {
	r.closed = true
	return r.closeErr
}

func stringsOfZero(length int) string {
	return string(bytes.Repeat([]byte{'0'}, length))
}
