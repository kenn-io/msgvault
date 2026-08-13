// Command mistral-probe-fixtures builds the private synthetic corpus consumed
// by `msgvault documents probe-mistral`. It uses only the Go standard library
// for open formats. Native binary formats must be supplied as synthetic seeds.
package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/documentindex/mistral"
)

const maxSeedBytes = int64(50 << 20)

// ZIP timestamps are encoded directly so fixtures remain byte-for-byte
// reproducible without adding extended timestamp fields. 33 is 1980-01-01 in
// the MS-DOS date representation used by ZIP.
const zipEpochDate = uint16(33)

const openDocumentStyles = `<office:document-styles xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:style="urn:oasis:names:tc:opendocument:xmlns:style:1.0" office:version="1.3"><office:styles/></office:document-styles>`

var nativeSeedFormats = []string{"doc", "ppt", "xls", "numbers", "msg"}

type zipEntry struct {
	name   string
	value  string
	stored bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("mistral-probe-fixtures", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var outputDirectory string
	var seedDirectory string
	flags.StringVar(&outputDirectory, "output", "", "private output directory")
	flags.StringVar(&seedDirectory, "seed-dir", "", "private directory containing native-format seeds")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse fixture builder flags: %w", err)
	}
	if flags.NArg() != 0 || outputDirectory == "" {
		return errors.New("usage: mistral-probe-fixtures --output <private-dir> [--seed-dir <private-dir>]")
	}
	created, missing, err := buildFixtureDirectory(outputDirectory, seedDirectory)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"fixture matrix remains incomplete: supply synthetic native seeds named %s through --seed-dir",
			strings.Join(missing, ", "),
		)
	}
	_, _ = fmt.Fprintf(stdout,
		"Validated %d Mistral probe fixtures in a private directory (%d created, no provider requests).\n",
		len(mistral.CandidateFormats()), created)
	return nil
}

func buildFixtureDirectory(outputDirectory, seedDirectory string) (int, []string, error) {
	if err := ensurePrivateDirectory(outputDirectory); err != nil {
		return 0, nil, fmt.Errorf("prepare fixture output: %w", err)
	}
	if seedDirectory != "" {
		info, err := os.Lstat(seedDirectory)
		if err != nil {
			return 0, nil, fmt.Errorf("inspect fixture seed directory: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return 0, nil, errors.New("fixture seed path must be a real directory")
		}
	}

	created := 0
	missing := make([]string, 0, len(nativeSeedFormats))
	for _, candidate := range mistral.CandidateFormats() {
		target := filepath.Join(outputDirectory, candidate.ID)
		if _, err := os.Lstat(target); err == nil {
			if err := validateFixture(target, candidate); err != nil {
				return created, missing, fmt.Errorf("validate existing fixture %q: %w", candidate.ID, err)
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return created, missing, fmt.Errorf("inspect fixture %q: %w", candidate.ID, err)
		}

		if seedDirectory != "" {
			seed := filepath.Join(seedDirectory, candidate.ID)
			if _, err := os.Lstat(seed); err == nil {
				if err := copyFixture(seed, target); err != nil {
					return created, missing, fmt.Errorf("copy fixture seed %q: %w", candidate.ID, err)
				}
				if err := validateFixture(target, candidate); err != nil {
					_ = os.Remove(target)
					return created, missing, fmt.Errorf("validate fixture seed %q: %w", candidate.ID, err)
				}
				created++
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return created, missing, fmt.Errorf("inspect fixture seed %q: %w", candidate.ID, err)
			}
		}

		content, generated, err := generatedFixture(candidate.ID)
		if err != nil {
			return created, missing, fmt.Errorf("generate fixture %q: %w", candidate.ID, err)
		}
		if !generated {
			missing = append(missing, candidate.ID)
			continue
		}
		if err := writeNewPrivateFile(target, bytes.NewReader(content), int64(len(content))); err != nil {
			return created, missing, fmt.Errorf("write fixture %q: %w", candidate.ID, err)
		}
		if err := validateFixture(target, candidate); err != nil {
			_ = os.Remove(target)
			return created, missing, fmt.Errorf("validate generated fixture %q: %w", candidate.ID, err)
		}
		created++
	}
	slices.Sort(missing)
	return created, missing, nil
}

func ensurePrivateDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(directory, 0o700); err != nil { // #nosec G301 -- private fixture directory.
			return err
		}
		return os.Chmod(directory, 0o700) // #nosec G302 -- private fixture directory.
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("fixture output path must be a real directory")
	}
	return os.Chmod(directory, 0o700) // #nosec G302 -- private fixture directory.
}

func copyFixture(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxSeedBytes {
		return errors.New("fixture seed must be a bounded regular non-symlink file")
	}
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return writeNewPrivateFile(target, io.LimitReader(file, maxSeedBytes+1), info.Size())
}

func writeNewPrivateFile(target string, reader io.Reader, expectedSize int64) error {
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- target is a fixed candidate ID under an operator-selected directory.
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written != expectedSize {
		_ = os.Remove(target)
		var sizeErr error
		if written != expectedSize {
			sizeErr = fmt.Errorf("fixture write size %d, want %d", written, expectedSize)
		}
		return errors.Join(copyErr, closeErr, sizeErr)
	}
	return nil
}

func validateFixture(path string, candidate mistral.CandidateFormat) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxSeedBytes {
		return errors.New("fixture must be a bounded regular non-symlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	detected, err := mistral.DetectFormat(file, info.Size(), candidate.MediaType)
	if err != nil {
		return err
	}
	if detected.ID != candidate.ID {
		return fmt.Errorf("detected %q, want %q", detected.ID, candidate.ID)
	}
	return nil
}

func generatedFixture(id string) ([]byte, bool, error) {
	sentinel, err := mistral.ProbeFixtureSentinel(id)
	if err != nil {
		return nil, false, err
	}
	xmlSentinel := escapeXML(sentinel)
	switch id {
	case "pdf":
		return pdfFixture(sentinel), true, nil
	case "docx":
		return zipFixture([]zipEntry{
			{name: "[Content_Types].xml", value: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`},
			{name: "_rels/.rels", value: `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`},
			{name: "word/document.xml", value: `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` + xmlSentinel + `</w:t></w:r></w:p><w:sectPr/></w:body></w:document>`},
		})
	case "odt":
		return zipFixture([]zipEntry{
			{name: "mimetype", value: "application/vnd.oasis.opendocument.text", stored: true},
			{name: "META-INF/manifest.xml", value: `<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.3"><manifest:file-entry manifest:full-path="/" manifest:media-type="application/vnd.oasis.opendocument.text"/><manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/><manifest:file-entry manifest:full-path="styles.xml" manifest:media-type="text/xml"/></manifest:manifest>`},
			{name: "content.xml", value: `<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" office:version="1.3"><office:body><office:text><text:p>` + xmlSentinel + `</text:p></office:text></office:body></office:document-content>`},
			{name: "styles.xml", value: openDocumentStyles},
		})
	case "rtf":
		return []byte(`{\rtf1\ansi ` + sentinel + `}`), true, nil
	case "pptx":
		return zipFixture([]zipEntry{
			{name: "[Content_Types].xml", value: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/><Override PartName="/ppt/slides/slide1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/></Types>`},
			{name: "_rels/.rels", value: `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/></Relationships>`},
			{name: "ppt/presentation.xml", value: `<p:presentation xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:sldIdLst><p:sldId id="256" r:id="rId1"/></p:sldIdLst><p:sldSz cx="9144000" cy="6858000"/></p:presentation>`},
			{name: "ppt/_rels/presentation.xml.rels", value: `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide1.xml"/></Relationships>`},
			{name: "ppt/slides/slide1.xml", value: `<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/><p:sp><p:nvSpPr><p:cNvPr id="2" name="Probe"/><p:cNvSpPr/><p:nvPr/></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:rPr lang="en-US"/><a:t>` + xmlSentinel + `</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`},
		})
	case "xlsx":
		return zipFixture([]zipEntry{
			{name: "[Content_Types].xml", value: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/></Types>`},
			{name: "_rels/.rels", value: `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
			{name: "xl/workbook.xml", value: `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Probe" sheetId="1" r:id="rId1"/></sheets></workbook>`},
			{name: "xl/_rels/workbook.xml.rels", value: `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`},
			{name: "xl/worksheets/sheet1.xml", value: `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>` + xmlSentinel + `</t></is></c></row></sheetData></worksheet>`},
		})
	case "ods":
		return zipFixture([]zipEntry{
			{name: "mimetype", value: "application/vnd.oasis.opendocument.spreadsheet", stored: true},
			{name: "META-INF/manifest.xml", value: `<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.3"><manifest:file-entry manifest:full-path="/" manifest:media-type="application/vnd.oasis.opendocument.spreadsheet"/><manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/><manifest:file-entry manifest:full-path="styles.xml" manifest:media-type="text/xml"/></manifest:manifest>`},
			{name: "content.xml", value: `<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" office:version="1.3"><office:body><office:spreadsheet><table:table table:name="Probe"><table:table-row><table:table-cell office:value-type="string"><text:p>` + xmlSentinel + `</text:p></table:table-cell></table:table-row></table:table></office:spreadsheet></office:body></office:document-content>`},
			{name: "styles.xml", value: openDocumentStyles},
		})
	case "csv":
		return []byte("kind,value\nprobe,\"" + sentinel + "\"\n"), true, nil
	case "epub":
		return zipFixture([]zipEntry{
			{name: "mimetype", value: "application/epub+zip", stored: true},
			{name: "META-INF/container.xml", value: `<?xml version="1.0"?><container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`},
			{name: "OEBPS/content.opf", value: `<?xml version="1.0"?><package version="3.0" unique-identifier="id" xmlns="http://www.idpf.org/2007/opf"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:identifier id="id">urn:uuid:00000000-0000-0000-0000-000000000001</dc:identifier><dc:title>Probe</dc:title><dc:language>en</dc:language></metadata><manifest><item id="chapter" href="chapter.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="chapter"/></spine></package>`},
			{name: "OEBPS/chapter.xhtml", value: `<html xmlns="http://www.w3.org/1999/xhtml"><head><title>Probe</title></head><body><p>` + xmlSentinel + `</p></body></html>`},
		})
	case "txt":
		return []byte(sentinel + "\n"), true, nil
	case "markdown":
		return []byte("# Probe\n\n" + sentinel + "\n"), true, nil
	case "rst":
		return []byte("Probe\n=====\n\n" + sentinel + "\n"), true, nil
	case "latex":
		return []byte(`\documentclass{article}\begin{document}` + sentinel + `\end{document}`), true, nil
	case "json":
		return []byte(`{"probe":"` + sentinel + `"}`), true, nil
	case "jsonl":
		return []byte(`{"kind":"probe","value":"` + sentinel + `"}` + "\n"), true, nil
	case "xml":
		return []byte(`<probe>` + xmlSentinel + `</probe>`), true, nil
	case "yaml":
		return []byte("---\nprobe: \"" + sentinel + "\"\n"), true, nil
	case "go":
		return []byte("package probe\n\nconst sentinel = \"" + sentinel + "\"\n"), true, nil
	case "python":
		return []byte("sentinel = \"" + sentinel + "\"\n"), true, nil
	case "javascript":
		return []byte("const sentinel = \"" + sentinel + "\";\n"), true, nil
	case "eml":
		return []byte("From: probe@example.test\r\nTo: archive@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\nSubject: Synthetic probe\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + sentinel + "\r\n"), true, nil
	case "doc", "ppt", "xls", "numbers", "msg":
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("no fixture policy for %q", id)
	}
}

func zipFixture(entries []zipEntry) ([]byte, bool, error) {
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, entry := range entries {
		header := &zip.FileHeader{
			Name:         entry.name,
			Method:       zip.Deflate,
			ModifiedDate: zipEpochDate,
		}
		value := []byte(entry.value)
		if entry.stored {
			header.Method = zip.Store
			header.CRC32 = crc32.ChecksumIEEE(value)
			header.CompressedSize64 = uint64(len(value))
			header.UncompressedSize64 = uint64(len(value))
		}
		var part io.Writer
		var err error
		if entry.stored {
			// CreateRaw avoids a data descriptor and keeps package-mandated
			// first mimetype entries stored with no extra fields.
			part, err = writer.CreateRaw(header)
		} else {
			part, err = writer.CreateHeader(header)
		}
		if err != nil {
			return nil, false, fmt.Errorf("create fixture ZIP entry %q: %w", entry.name, err)
		}
		if _, err := part.Write(value); err != nil {
			return nil, false, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, false, fmt.Errorf("close fixture ZIP: %w", err)
	}
	return output.Bytes(), true, nil
}

func escapeXML(value string) string {
	var output bytes.Buffer
	for _, char := range value {
		switch char {
		case '&':
			output.WriteString("&amp;")
		case '<':
			output.WriteString("&lt;")
		case '>':
			output.WriteString("&gt;")
		default:
			output.WriteRune(char)
		}
	}
	return output.String()
}

func pdfFixture(sentinel string) []byte {
	stream := "BT /F1 12 Tf 72 720 Td (" + strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)").Replace(sentinel) + ") Tj ET"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
	}
	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}
