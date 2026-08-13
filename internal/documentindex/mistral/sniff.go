package mistral

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"os"
	"path"
	"strings"
	"unicode/utf8"
)

const (
	maxSniffBytes            = int64(8 << 20)
	maxTextSniffBytes        = int64(50 << 20)
	maxZIPEntries            = 10_000
	maxZIPCentralDirectory   = uint32(16 << 20)
	maxZIPExpandedBytes      = uint64(500 << 20)
	maxZIPSingleExpandedByte = uint64(100 << 20)
	ooxmlContentTypesName    = "[Content_Types].xml"
)

var (
	compoundFileMagic = []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}
)

const compoundNoStream = uint32(0xffffffff)

type compoundDirectoryEntry struct {
	name               string
	entryType          byte
	left, right, child uint32
}

// DetectFormat validates a provider candidate from bounded bytes. Declared
// type is a hint only: container families must prove internal markers, while
// inherently ambiguous text formats also require syntactically safe UTF-8.
func DetectFormat(reader io.ReaderAt, size int64, declaredMediaType string) (CandidateFormat, error) {
	if reader == nil || size <= 0 {
		return CandidateFormat{}, errors.New("document format detection requires nonempty bytes")
	}
	mediaType, parameters, err := mime.ParseMediaType(declaredMediaType)
	if err != nil || len(parameters) != 0 || mediaType != strings.ToLower(mediaType) {
		return CandidateFormat{}, errors.New("document format detection requires a canonical media type")
	}
	prefix, err := readPrefix(reader, size, maxSniffBytes)
	if err != nil {
		return CandidateFormat{}, err
	}

	var detected CandidateFormat
	switch {
	case bytes.HasPrefix(prefix, []byte("%PDF-")):
		detected, _ = CandidateFormatByID("pdf")
	case bytes.HasPrefix(prefix, []byte(`{\rtf`)):
		detected, _ = CandidateFormatByID("rtf")
	case bytes.HasPrefix(prefix, compoundFileMagic):
		detected, err = detectCompoundFormat(reader, size)
	case bytes.HasPrefix(prefix, []byte("PK\x03\x04")) || bytes.HasPrefix(prefix, []byte("PK\x05\x06")):
		detected, err = detectZIPFormat(reader, size)
	default:
		if size > maxTextSniffBytes {
			return CandidateFormat{}, errors.New("document text exceeds type-detection limit")
		}
		content, readErr := readPrefix(reader, size, maxTextSniffBytes)
		if readErr != nil {
			return CandidateFormat{}, readErr
		}
		detected, err = detectTextFormat(content, mediaType)
	}
	if err != nil {
		return CandidateFormat{}, err
	}
	if detected.MediaType != mediaType {
		return CandidateFormat{}, fmt.Errorf("document bytes are %s, not declared %s", detected.MediaType, mediaType)
	}
	return detected, nil
}

func detectCompoundFormat(reader io.ReaderAt, size int64) (CandidateFormat, error) {
	names, err := compoundDirectoryNames(reader, size)
	if err != nil {
		return CandidateFormat{}, err
	}
	ids := make([]string, 0, 2)
	if names["WordDocument"] {
		ids = append(ids, "doc")
	}
	if names["Workbook"] || names["Book"] {
		ids = append(ids, "xls")
	}
	if names["PowerPoint Document"] {
		ids = append(ids, "ppt")
	}
	if names["__properties_version1.0"] {
		ids = append(ids, "msg")
	}
	if len(ids) != 1 {
		return CandidateFormat{}, errors.New("compound document has missing or ambiguous family markers")
	}
	format, _ := CandidateFormatByID(ids[0])
	return format, nil
}

func compoundDirectoryNames(reader io.ReaderAt, size int64) (map[string]bool, error) {
	const (
		freeSector        = uint32(0xffffffff)
		endOfChain        = uint32(0xfffffffe)
		fatSector         = uint32(0xfffffffd)
		difatSector       = uint32(0xfffffffc)
		maxDIFATSectors   = 1_024
		maxDirectoryBytes = int64(8 << 20)
	)
	if size < 512 {
		return nil, errors.New("compound document header is truncated")
	}
	header := make([]byte, 512)
	if _, err := reader.ReadAt(header, 0); err != nil {
		return nil, fmt.Errorf("read compound document header: %w", err)
	}
	if !bytes.Equal(header[:8], compoundFileMagic) || binary.LittleEndian.Uint16(header[28:30]) != 0xfffe {
		return nil, errors.New("compound document header is invalid")
	}
	sectorShift := binary.LittleEndian.Uint16(header[30:32])
	majorVersion := binary.LittleEndian.Uint16(header[26:28])
	if (majorVersion != 3 || sectorShift != 9) && (majorVersion != 4 || sectorShift != 12) {
		return nil, errors.New("compound document sector size is unsupported")
	}
	sectorSize := int64(1 << sectorShift)
	sectorCount := size/sectorSize - 1
	if sectorCount <= 0 || size%sectorSize != 0 {
		return nil, errors.New("compound document size is invalid")
	}
	numFAT := int(binary.LittleEndian.Uint32(header[44:48]))
	firstDirectory := binary.LittleEndian.Uint32(header[48:52])
	firstDIFAT := binary.LittleEndian.Uint32(header[68:72])
	numDIFAT := int(binary.LittleEndian.Uint32(header[72:76]))
	if numFAT <= 0 || numFAT > int(sectorCount) || numDIFAT > maxDIFATSectors {
		return nil, errors.New("compound document allocation table exceeds limits")
	}
	fatSectors := make([]uint32, 0, numFAT)
	for offset := 76; offset < 512 && len(fatSectors) < numFAT; offset += 4 {
		sector := binary.LittleEndian.Uint32(header[offset : offset+4])
		if sector != freeSector {
			fatSectors = append(fatSectors, sector)
		}
	}
	seenDIFAT := map[uint32]bool{}
	for i, sector := 0, firstDIFAT; i < numDIFAT; i++ {
		if int64(sector) >= sectorCount || seenDIFAT[sector] {
			return nil, errors.New("compound document DIFAT chain is invalid")
		}
		seenDIFAT[sector] = true
		data, readErr := readCompoundSector(reader, sector, sectorSize, size)
		if readErr != nil {
			return nil, readErr
		}
		for offset := 0; offset < len(data)-4 && len(fatSectors) < numFAT; offset += 4 {
			fatID := binary.LittleEndian.Uint32(data[offset : offset+4])
			if fatID != freeSector {
				fatSectors = append(fatSectors, fatID)
			}
		}
		sector = binary.LittleEndian.Uint32(data[len(data)-4:])
		if i == numDIFAT-1 && sector != endOfChain {
			return nil, errors.New("compound document DIFAT chain does not terminate")
		}
	}
	if len(fatSectors) != numFAT {
		return nil, errors.New("compound document FAT sector count is invalid")
	}
	fat := make([]uint32, 0, int(sectorCount))
	seenFAT := map[uint32]bool{}
	for _, sector := range fatSectors {
		if int64(sector) >= sectorCount || seenFAT[sector] {
			return nil, errors.New("compound document FAT sector list is invalid")
		}
		seenFAT[sector] = true
		data, readErr := readCompoundSector(reader, sector, sectorSize, size)
		if readErr != nil {
			return nil, readErr
		}
		for offset := 0; offset < len(data); offset += 4 {
			fat = append(fat, binary.LittleEndian.Uint32(data[offset:offset+4]))
		}
	}
	if len(fat) < int(sectorCount) {
		return nil, errors.New("compound document FAT is truncated")
	}
	for _, sector := range fatSectors {
		if fat[sector] != fatSector {
			return nil, errors.New("compound document FAT sector is not self-marked")
		}
	}
	for sector := range seenDIFAT {
		if fat[sector] != difatSector {
			return nil, errors.New("compound document DIFAT sector is not self-marked")
		}
	}

	var directory bytes.Buffer
	seenDirectory := map[uint32]bool{}
	for sector := firstDirectory; sector != endOfChain; {
		if int64(sector) >= sectorCount || seenDirectory[sector] || int64(directory.Len())+sectorSize > maxDirectoryBytes {
			return nil, errors.New("compound document directory chain exceeds limits")
		}
		seenDirectory[sector] = true
		data, readErr := readCompoundSector(reader, sector, sectorSize, size)
		if readErr != nil {
			return nil, readErr
		}
		_, _ = directory.Write(data)
		next := fat[sector]
		if next == freeSector || next == fatSector || next == difatSector {
			return nil, errors.New("compound document directory chain is invalid")
		}
		sector = next
	}
	entries := make([]compoundDirectoryEntry, 0, directory.Len()/128)
	rootIndex := -1
	data := directory.Bytes()
	for offset := 0; offset+128 <= len(data); offset += 128 {
		entry := data[offset : offset+128]
		entryType := entry[66]
		if entryType == 0 {
			entries = append(entries, compoundDirectoryEntry{})
			continue
		}
		if entryType != 1 && entryType != 2 && entryType != 5 {
			return nil, errors.New("compound document directory entry type is invalid")
		}
		nameLength := int(binary.LittleEndian.Uint16(entry[64:66]))
		if nameLength < 2 || nameLength > 64 || nameLength%2 != 0 {
			return nil, errors.New("compound document directory name is invalid")
		}
		name, decodeErr := decodeUTF16LE(entry[:nameLength-2])
		if decodeErr != nil || name == "" {
			return nil, errors.New("compound document directory name is invalid")
		}
		entries = append(entries, compoundDirectoryEntry{
			name: name, entryType: entryType,
			left: binary.LittleEndian.Uint32(entry[68:72]), right: binary.LittleEndian.Uint32(entry[72:76]),
			child: binary.LittleEndian.Uint32(entry[76:80]),
		})
		if entryType == 5 {
			if rootIndex != -1 {
				return nil, errors.New("compound document has multiple root entries")
			}
			rootIndex = len(entries) - 1
		}
	}
	if rootIndex < 0 {
		return nil, errors.New("compound document has no root entry")
	}
	names := map[string]bool{}
	seen := map[uint32]bool{}
	stack := []uint32{entries[rootIndex].child}
	for len(stack) > 0 {
		index := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if index == compoundNoStream {
			continue
		}
		if uint64(index) >= uint64(len(entries)) || seen[index] || entries[index].entryType == 0 || entries[index].entryType == 5 {
			return nil, errors.New("compound document root directory tree is invalid")
		}
		seen[index] = true
		entry := entries[index]
		if entry.entryType == 2 {
			names[entry.name] = true
		}
		stack = append(stack, entry.left, entry.right)
	}
	return names, nil
}

func readCompoundSector(reader io.ReaderAt, sector uint32, sectorSize, size int64) ([]byte, error) {
	offset := (int64(sector) + 1) * sectorSize
	if offset < sectorSize || offset > size-sectorSize {
		return nil, errors.New("compound document sector is out of range")
	}
	data := make([]byte, sectorSize)
	if _, err := reader.ReadAt(data, offset); err != nil {
		return nil, fmt.Errorf("read compound document sector: %w", err)
	}
	return data, nil
}

func decodeUTF16LE(data []byte) (string, error) {
	if len(data)%2 != 0 {
		return "", errors.New("odd UTF-16 length")
	}
	runes := make([]rune, 0, len(data)/2)
	for i := 0; i < len(data); i += 2 {
		value := binary.LittleEndian.Uint16(data[i : i+2])
		if value == 0 || value >= 0xd800 && value <= 0xdfff {
			return "", errors.New("unsupported UTF-16 directory name")
		}
		runes = append(runes, rune(value))
	}
	return string(runes), nil
}

func detectZIPFormat(reader io.ReaderAt, size int64) (CandidateFormat, error) {
	if err := validateZIPEndRecord(reader, size); err != nil {
		return CandidateFormat{}, err
	}
	archive, err := zip.NewReader(reader, size)
	if err != nil {
		return CandidateFormat{}, fmt.Errorf("open document ZIP container: %w", err)
	}
	if len(archive.File) > maxZIPEntries {
		return CandidateFormat{}, errors.New("document ZIP container has too many entries")
	}
	names := make(map[string]bool, len(archive.File))
	var expanded uint64
	var mimeValue string
	var contentTypes []byte
	for _, entry := range archive.File {
		if err := validateZIPName(entry.Name); err != nil {
			return CandidateFormat{}, err
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			return CandidateFormat{}, errors.New("document ZIP container contains a symlink")
		}
		if entry.Flags&1 != 0 || (entry.Method != zip.Store && entry.Method != zip.Deflate) {
			return CandidateFormat{}, errors.New("document ZIP container uses unsupported encryption or compression")
		}
		if entry.UncompressedSize64 > maxZIPSingleExpandedByte || expanded > maxZIPExpandedBytes-entry.UncompressedSize64 {
			return CandidateFormat{}, errors.New("document ZIP container exceeds expanded-byte limits")
		}
		expanded += entry.UncompressedSize64
		if names[entry.Name] {
			return CandidateFormat{}, errors.New("document ZIP container has duplicate entry names")
		}
		names[entry.Name] = true
		if err := verifyZIPEntry(entry); err != nil {
			return CandidateFormat{}, err
		}
		if entry.Name == "mimetype" {
			value, readErr := readZIPEntry(entry, 256)
			if readErr != nil {
				return CandidateFormat{}, readErr
			}
			mimeValue = string(value)
		}
		if entry.Name == ooxmlContentTypesName {
			value, readErr := readZIPEntry(entry, 2<<20)
			if readErr != nil {
				return CandidateFormat{}, readErr
			}
			contentTypes = value
		}
	}

	var id string
	switch {
	case names["word/document.xml"] && hasOOXMLMainType(contentTypes, "/word/document.xml",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"):
		id = "docx"
	case names["ppt/presentation.xml"] && hasOOXMLMainType(contentTypes, "/ppt/presentation.xml",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"):
		id = "pptx"
	case names["xl/workbook.xml"] && hasOOXMLMainType(contentTypes, "/xl/workbook.xml",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"):
		id = "xlsx"
	case mimeValue == "application/vnd.oasis.opendocument.text" && names["META-INF/manifest.xml"]:
		id = "odt"
	case mimeValue == "application/vnd.oasis.opendocument.spreadsheet" && names["META-INF/manifest.xml"]:
		id = "ods"
	case mimeValue == "application/epub+zip" && names["META-INF/container.xml"]:
		id = "epub"
	case hasNumbersMarker(names):
		id = "numbers"
	default:
		return CandidateFormat{}, errors.New("zip container is not a supported document format")
	}
	format, _ := CandidateFormatByID(id)
	return format, nil
}

func hasOOXMLMainType(content []byte, partName, contentType string) bool {
	if len(content) == 0 || !validXMLDocument(content) {
		return false
	}
	var document struct {
		XMLName   xml.Name `xml:"Types"`
		Overrides []struct {
			PartName    string `xml:"PartName,attr"`
			ContentType string `xml:"ContentType,attr"`
		} `xml:"Override"`
	}
	if err := xml.Unmarshal(content, &document); err != nil ||
		document.XMLName.Space != "http://schemas.openxmlformats.org/package/2006/content-types" {
		return false
	}
	found := false
	for _, override := range document.Overrides {
		if override.PartName != partName {
			continue
		}
		if found || override.ContentType != contentType {
			return false
		}
		found = true
	}
	return found
}

func validateZIPEndRecord(reader io.ReaderAt, size int64) error {
	const maxTail = int64(65_557)
	tailSize := min(size, maxTail)
	tail := make([]byte, tailSize)
	if _, err := reader.ReadAt(tail, size-tailSize); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read document ZIP end record: %w", err)
	}
	signature := []byte{'P', 'K', 0x05, 0x06}
	offset := bytes.LastIndex(tail, signature)
	if offset < 0 || len(tail)-offset < 22 {
		return errors.New("document ZIP container has no bounded end record")
	}
	record := tail[offset:]
	entries := binary.LittleEndian.Uint16(record[10:12])
	entriesOnDisk := binary.LittleEndian.Uint16(record[8:10])
	centralSize := binary.LittleEndian.Uint32(record[12:16])
	centralOffset := binary.LittleEndian.Uint32(record[16:20])
	commentSize := int(binary.LittleEndian.Uint16(record[20:22]))
	if binary.LittleEndian.Uint16(record[4:6]) != 0 || binary.LittleEndian.Uint16(record[6:8]) != 0 || entriesOnDisk != entries {
		return errors.New("multi-disk document ZIP containers are unsupported")
	}
	if entries == 0xffff || centralSize == 0xffffffff || centralOffset == 0xffffffff ||
		int(entries) > maxZIPEntries || centralSize > maxZIPCentralDirectory {
		return errors.New("document ZIP central directory exceeds limits")
	}
	if int64(centralOffset)+int64(centralSize) > size {
		return errors.New("document ZIP central directory is out of range")
	}
	if len(record) != 22+commentSize {
		return errors.New("document ZIP end record has invalid comment length")
	}
	return nil
}

func validateZIPName(name string) error {
	if name == "" || strings.ContainsRune(name, 0) || strings.ContainsAny(name, "\\:") || strings.HasPrefix(name, "/") {
		return errors.New("document ZIP container has an unsafe entry name")
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") || clean != strings.TrimSuffix(name, "/") {
		return errors.New("document ZIP container has a traversing entry name")
	}
	return nil
}

func readZIPEntry(entry *zip.File, limit int64) ([]byte, error) {
	if limit < 0 || entry.UncompressedSize64 > maxZIPSingleExpandedByte || int64(entry.UncompressedSize64) > limit {
		return nil, errors.New("document ZIP marker entry exceeds limit")
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("open document ZIP marker: %w", err)
	}
	defer func() { _ = reader.Close() }()
	value, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read document ZIP marker: %w", err)
	}
	if int64(len(value)) > limit {
		return nil, errors.New("document ZIP marker entry exceeds limit")
	}
	return value, nil
}

func verifyZIPEntry(entry *zip.File) error {
	if entry.UncompressedSize64 > maxZIPSingleExpandedByte {
		return errors.New("document ZIP entry exceeds verification limit")
	}
	reader, err := entry.Open()
	if err != nil {
		return fmt.Errorf("open document ZIP entry: %w", err)
	}
	expectedSize := int64(entry.UncompressedSize64)
	written, readErr := io.Copy(io.Discard, io.LimitReader(reader, expectedSize+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || written != expectedSize {
		return errors.New("document ZIP entry failed bounded verification")
	}
	return nil
}

func hasNumbersMarker(names map[string]bool) bool {
	for name := range names {
		if strings.HasPrefix(name, "Index/Tables/") && strings.HasSuffix(name, ".iwa") {
			return true
		}
	}
	return false
}

func detectTextFormat(content []byte, mediaType string) (CandidateFormat, error) {
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return CandidateFormat{}, errors.New("document text is not safe UTF-8")
	}
	candidate, ok := candidateByMediaType(mediaType)
	if !ok || !isTextCandidate(candidate) {
		return CandidateFormat{}, errors.New("document bytes have no supported signature")
	}
	trimmed := bytes.TrimSpace(content)
	switch candidate.ID {
	case "json":
		if len(trimmed) == 0 || !json.Valid(trimmed) {
			return CandidateFormat{}, errors.New("declared JSON document is invalid")
		}
	case "jsonl":
		for line := range bytes.SplitSeq(trimmed, []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) > 0 && !json.Valid(bytes.TrimSpace(line)) {
				return CandidateFormat{}, errors.New("declared JSONL document is invalid")
			}
		}
	case "xml":
		if !validXMLDocument(content) {
			return CandidateFormat{}, errors.New("declared XML document is invalid")
		}
	case "csv":
		csvReader := csv.NewReader(bytes.NewReader(content))
		csvReader.FieldsPerRecord = -1
		if records, err := csvReader.ReadAll(); err != nil || len(records) == 0 {
			return CandidateFormat{}, errors.New("declared CSV document is invalid")
		}
	case "latex":
		if !bytes.Contains(content, []byte(`\documentclass`)) && !bytes.Contains(content, []byte(`\begin{document}`)) {
			return CandidateFormat{}, errors.New("declared LaTeX document has no document marker")
		}
	case "eml":
		message, err := mail.ReadMessage(bytes.NewReader(content))
		if err != nil || message.Header.Get("From") == "" || message.Header.Get("Date") == "" {
			return CandidateFormat{}, errors.New("declared EML document lacks required message headers")
		}
	case "yaml":
		if len(trimmed) == 0 || (!bytes.HasPrefix(trimmed, []byte("---")) && !bytes.Contains(trimmed, []byte(": "))) {
			return CandidateFormat{}, errors.New("declared YAML document has no structural marker")
		}
	}
	return candidate, nil
}

func validXMLDocument(content []byte) bool {
	decoder := xml.NewDecoder(bytes.NewReader(content))
	depth := 0
	roots := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return roots == 1 && depth == 0
		}
		if err != nil {
			return false
		}
		switch value := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots > 1 {
					return false
				}
			}
			depth++
		case xml.EndElement:
			depth--
			if depth < 0 {
				return false
			}
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(value)) != 0 {
				return false
			}
		}
	}
}

func isTextCandidate(candidate CandidateFormat) bool {
	switch candidate.Family {
	case "text", "structured", "source", "mail", "spreadsheet":
		return candidate.ID != "msg" && candidate.ID != "xls" && candidate.ID != "xlsx" && candidate.ID != "ods" && candidate.ID != "numbers"
	default:
		return false
	}
}

func candidateByMediaType(mediaType string) (CandidateFormat, bool) {
	for _, candidate := range candidateFormats {
		if candidate.MediaType == mediaType {
			return candidate, true
		}
	}
	return CandidateFormat{}, false
}

func readPrefix(reader io.ReaderAt, size, limit int64) ([]byte, error) {
	length := min(size, limit)
	buffer := make([]byte, length)
	read, err := reader.ReadAt(buffer, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read document signature: %w", err)
	}
	if int64(read) != length {
		return nil, errors.New("document bytes changed during signature read")
	}
	return buffer, nil
}
