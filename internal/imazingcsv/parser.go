package imazingcsv

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var requiredHeaders = []string{"chat session", "message date", "service", "type"}

var supportedDelimiters = []rune{',', '\t', ';'}

// Discover resolves an iMazing export root or a directory containing CSV files.
func Discover(path string) (Layout, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Layout{}, fmt.Errorf("resolve export directory: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Layout{}, fmt.Errorf("inspect export directory %s: %w", abs, err)
	}
	if !info.IsDir() {
		return Layout{}, fmt.Errorf("iMazing export path %s is not a directory", abs)
	}

	root := filepath.Dir(abs)
	csvDir := abs
	if childInfo, childErr := os.Stat(filepath.Join(abs, "csv")); childErr == nil && childInfo.IsDir() {
		root = abs
		csvDir = filepath.Join(abs, "csv")
	}
	entries, err := os.ReadDir(csvDir)
	if err != nil {
		return Layout{}, fmt.Errorf("read CSV directory %s: %w", csvDir, err)
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".csv") {
			continue
		}
		files = append(files, filepath.Join(csvDir, entry.Name()))
	}
	sort.Slice(files, func(i, j int) bool {
		left := strings.ToLower(filepath.Base(files[i]))
		right := strings.ToLower(filepath.Base(files[j]))
		if left == right {
			return files[i] < files[j]
		}
		return left < right
	})
	if len(files) == 0 {
		return Layout{}, fmt.Errorf("no CSV files found in %s", csvDir)
	}
	return Layout{
		Root:           root,
		CSVDir:         csvDir,
		AttachmentsDir: filepath.Join(root, "attachments"),
		CSVFiles:       files,
	}, nil
}

// ParseFiles parses all discovered files in deterministic order.
func ParseFiles(ctx context.Context, layout Layout, timezone string) ([]Row, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("load timezone %q: %w", timezone, err)
	}
	var rows []Row
	for _, path := range layout.CSVFiles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parsed, err := parseFile(ctx, path, loc)
		if err != nil {
			return nil, err
		}
		rows = append(rows, parsed...)
	}
	return rows, nil
}

func parseFile(ctx context.Context, path string, loc *time.Location) ([]Row, error) {
	delimiter, err := detectDelimiter(path)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = file.Close() }()

	reader := csv.NewReader(withoutBOM(newValidUTF8Reader(file)))
	reader.Comma = delimiter
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("parse %s record 1: %w", filepath.Base(path), err)
	}
	indexes, originals, err := validateHeader(header)
	if err != nil {
		return nil, fmt.Errorf("parse %s record 1: %w", filepath.Base(path), err)
	}
	reader.FieldsPerRecord = len(header)

	var rows []Row
	for record := 2; ; record++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		values, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("parse %s record %d: %w", filepath.Base(path), record, readErr)
		}
		row, rowErr := normalizeRow(values, indexes, originals, filepath.Base(path), record, loc)
		if rowErr != nil {
			return nil, fmt.Errorf("parse %s record %d: %w", filepath.Base(path), record, rowErr)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func detectDelimiter(path string) (rune, error) {
	for _, delimiter := range supportedDelimiters {
		file, err := os.Open(path)
		if err != nil {
			return 0, fmt.Errorf("open %s: %w", filepath.Base(path), err)
		}
		reader := csv.NewReader(withoutBOM(newValidUTF8Reader(file)))
		reader.Comma = delimiter
		reader.FieldsPerRecord = -1
		header, readErr := reader.Read()
		_ = file.Close()
		if readErr != nil {
			if errors.Is(readErr, errInvalidUTF8) {
				return 0, fmt.Errorf("parse %s record 1: %w", filepath.Base(path), readErr)
			}
			continue
		}
		if hasRequiredHeaders(header) {
			return delimiter, nil
		}
	}
	return 0, fmt.Errorf("parse %s record 1: required columns Chat Session, Message Date, Service, and Type were not found", filepath.Base(path))
}

func hasRequiredHeaders(header []string) bool {
	present := make(map[string]bool, len(header))
	for _, value := range header {
		present[normalizeHeader(value)] = true
	}
	for _, required := range requiredHeaders {
		if !present[required] {
			return false
		}
	}
	return true
}

func validateHeader(header []string) (map[string]int, []string, error) {
	indexes := make(map[string]int, len(header))
	originals := make([]string, len(header))
	for i, value := range header {
		original := strings.TrimSpace(value)
		normalized := normalizeHeader(value)
		if _, exists := indexes[normalized]; exists {
			return nil, nil, fmt.Errorf("duplicate header %q", original)
		}
		indexes[normalized] = i
		originals[i] = original
	}
	for _, required := range requiredHeaders {
		if _, ok := indexes[required]; !ok {
			return nil, nil, fmt.Errorf("required column %q is missing", required)
		}
	}
	return indexes, originals, nil
}

func normalizeHeader(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(value, "\ufeff")))
}

func normalizeRow(values []string, indexes map[string]int, originals []string, file string, record int, loc *time.Location) (Row, error) {
	raw := make(map[string]string, len(values))
	for i, value := range values {
		raw[originals[i]] = value
	}
	value := func(name string) string {
		index, ok := indexes[name]
		if !ok {
			return ""
		}
		return strings.TrimSpace(values[index])
	}
	rawValue := func(name string) string {
		index, ok := indexes[name]
		if !ok {
			return ""
		}
		return values[index]
	}

	chatSession := value("chat session")
	if chatSession == "" {
		return Row{}, errors.New("chat session is empty")
	}
	messageDate := value("message date")
	sentAt, err := parseMessageTime(messageDate, loc)
	if err != nil {
		return Row{}, fmt.Errorf("invalid message date %q: %w", messageDate, err)
	}
	direction, err := parseDirection(value("type"))
	if err != nil {
		return Row{}, err
	}
	service, err := normalizeService(value("service"))
	if err != nil {
		return Row{}, err
	}
	deliveredTime, hasDeliveredTime, err := parseOptionalTime(value("delivered date"), loc)
	if err != nil {
		return Row{}, fmt.Errorf("invalid delivered date %q: %w", value("delivered date"), err)
	}
	readTime, hasReadTime, err := parseOptionalTime(value("read date"), loc)
	if err != nil {
		return Row{}, fmt.Errorf("invalid read date %q: %w", value("read date"), err)
	}
	var deliveredAt, readAt *time.Time
	if hasDeliveredTime {
		deliveredAt = &deliveredTime
	}
	if hasReadTime {
		readAt = &readTime
	}

	return Row{
		ChatSession:    chatSession,
		MessageDate:    messageDate,
		DeliveredDate:  value("delivered date"),
		ReadDate:       value("read date"),
		Service:        service,
		SenderID:       value("sender id"),
		SenderName:     value("sender name"),
		Status:         value("status"),
		ReplyingTo:     rawValue("replying to"),
		Subject:        rawValue("subject"),
		Text:           rawValue("text"),
		Attachment:     value("attachment"),
		AttachmentType: value("attachment type"),
		Direction:      direction,
		SentAt:         sentAt,
		DeliveredAt:    deliveredAt,
		ReadAt:         readAt,
		Raw:            raw,
		File:           file,
		Record:         record,
	}, nil
}

func parseDirection(value string) (Direction, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "incoming":
		return DirectionIncoming, nil
	case "outgoing":
		return DirectionOutgoing, nil
	default:
		return "", fmt.Errorf("unsupported direction %q", value)
	}
}

func normalizeService(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", errors.New("service is empty")
	}
	switch strings.ToLower(trimmed) {
	case "imessage":
		return "imessage", nil
	case "sms":
		return "sms", nil
	case "rcs":
		return "rcs", nil
	}
	var result strings.Builder
	separator := false
	for _, r := range strings.ToLower(trimmed) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if separator && result.Len() > 0 {
				result.WriteByte('-')
			}
			result.WriteRune(r)
			separator = false
		} else {
			separator = true
		}
	}
	slug := strings.Trim(result.String(), "-")
	if slug == "" {
		return "", fmt.Errorf("service %q has no usable name", value)
	}
	return slug, nil
}

func parseOptionalTime(value string, loc *time.Location) (time.Time, bool, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, false, nil
	}
	parsed, err := parseMessageTime(value, loc)
	if err != nil {
		return time.Time{}, false, err
	}
	return parsed, true, nil
}

func parseMessageTime(value string, loc *time.Location) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if parsed, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
		return parsed.UTC(), nil
	}
	return ParseWallTime(trimmed, loc)
}

// ParseWallTime parses an offset-free iMazing timestamp. During a fall-back
// overlap it chooses the earlier instant; it rejects spring-forward gaps.
func ParseWallTime(value string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		return time.Time{}, errors.New("timezone is required")
	}
	var wall time.Time
	var parseErr error
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
		wall, parseErr = time.ParseInLocation(layout, value, time.UTC)
		if parseErr == nil {
			break
		}
	}
	if parseErr != nil {
		return time.Time{}, fmt.Errorf("parse wall time: %w", parseErr)
	}

	guess := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), wall.Nanosecond(), loc)
	offsets := make(map[int]struct{})
	for hour := -48; hour <= 48; hour++ {
		_, offset := guess.Add(time.Duration(hour) * time.Hour).Zone()
		offsets[offset] = struct{}{}
	}
	var candidates []time.Time
	seen := make(map[int64]struct{})
	for offset := range offsets {
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		local := candidate.In(loc)
		if local.Year() != wall.Year() || local.Month() != wall.Month() || local.Day() != wall.Day() ||
			local.Hour() != wall.Hour() || local.Minute() != wall.Minute() || local.Second() != wall.Second() ||
			local.Nanosecond() != wall.Nanosecond() {
			continue
		}
		if _, ok := seen[candidate.UnixNano()]; ok {
			continue
		}
		seen[candidate.UnixNano()] = struct{}{}
		candidates = append(candidates, candidate.UTC())
	}
	if len(candidates) == 0 {
		return time.Time{}, fmt.Errorf("nonexistent wall time in %s", loc)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Before(candidates[j]) })
	return candidates[0], nil
}

var errInvalidUTF8 = errors.New("invalid UTF-8")

type validUTF8Reader struct {
	reader  *bufio.Reader
	pending []byte
	err     error
}

func newValidUTF8Reader(reader io.Reader) io.Reader {
	return &validUTF8Reader{reader: bufio.NewReader(reader)}
}

func (r *validUTF8Reader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	written := 0
	for written < len(buffer) {
		if len(r.pending) > 0 {
			copied := copy(buffer[written:], r.pending)
			r.pending = r.pending[copied:]
			written += copied
			continue
		}
		if r.err != nil {
			if written > 0 {
				return written, nil
			}
			return 0, r.err
		}
		runeValue, size, err := r.reader.ReadRune()
		if err != nil {
			r.err = err
			continue
		}
		if runeValue == utf8.RuneError && size == 1 {
			r.err = errInvalidUTF8
			continue
		}
		var encoded [utf8.UTFMax]byte
		encodedSize := utf8.EncodeRune(encoded[:], runeValue)
		r.pending = append(r.pending, encoded[:encodedSize]...)
	}
	return written, nil
}

func withoutBOM(reader io.Reader) io.Reader {
	buffered := bufio.NewReader(reader)
	if prefix, err := buffered.Peek(3); err == nil && string(prefix) == "\ufeff" {
		_, _ = buffered.Discard(3)
	}
	return buffered
}
