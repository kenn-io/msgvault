package whatsapp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/imazingcsv"
)

// The supported subset is intentional: numeric dates, comma, colon clocks,
// and either Android's dash or iOS's brackets. Never guess date order.
var textHeader = regexp.MustCompile(`^(\[?)([0-9]{1,4}[/.-][0-9]{1,2}[/.-][0-9]{1,4}), ([0-9]{1,2}:[0-9]{2}(?::[0-9]{2})?(?:[ \x{00a0}\x{202f}]+(?:[AaPp][Mm]|[AaPp]\. ?[Mm]\.))?)(\] | - )(.*)$`)

// Record-shaped continuation lines cannot be distinguished from unfamiliar headers.
var textDateStart = regexp.MustCompile(`^(?:\[|[^\r\n]*\S[\s\p{Z}]+[-\p{Pd}][\s\p{Z}]+\S)`)
var textNotice = regexp.MustCompile(`^(?:Messages and calls are end-to-end encrypted$|Messages to this chat and calls are now secured with end-to-end encryption$|.+ changed (?:the group (?:description|name|icon)|the subject)(?:$| )|.+ created group "|You created (?:group|this group)(?:$| ))`)

func textLocation(order, zone string) (*time.Location, error) {
	if order != "dmy" && order != "mdy" && order != "ymd" {
		return nil, errors.New("--date-order must be dmy, mdy or ymd")
	}
	return imazingcsv.ValidateTimezone(zone)
}

func parseText(ctx context.Context, reader io.Reader, order string, loc *time.Location, limits textLimits) ([]textRecord, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limited := &io.LimitedReader{R: reader, N: limits.Bytes + 1}
	hash := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(limited, hash))
	// Allow the first-line BOM and CRLF outside the retained record byte limit.
	scanner.Buffer(make([]byte, 4096), limits.RecordBytes+5)
	var rows []textRecord
	var body, rawRecord strings.Builder
	flush := func() {
		if len(rows) > 0 {
			rows[len(rows)-1].Body = body.String()
			rows[len(rows)-1].Raw = rawRecord.String()
		}
	}
	line := 0
	for scanner.Scan() {
		line++
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if limited.N == 0 {
			return nil, "", fmt.Errorf("TXT exceeds %d byte limit", limits.Bytes)
		}
		raw := scanner.Text()
		if !utf8.ValidString(raw) {
			return nil, "", fmt.Errorf("line %d: TXT is not valid UTF-8", line)
		}
		if line == 1 {
			raw = strings.TrimPrefix(raw, "\ufeff")
		}
		header := strings.TrimLeft(raw, "\u200e\u200f\u202a\u202b\u202c")
		match := textHeader.FindStringSubmatch(header)
		if match == nil {
			if len(rows) == 0 || textDateStart.MatchString(header) {
				return nil, "", fmt.Errorf("line %d: unsupported or malformed TXT header", line)
			}
			row := &rows[len(rows)-1]
			if rawRecord.Len()+1+len(raw) > limits.RecordBytes {
				return nil, "", fmt.Errorf("line %d: record exceeds %d bytes", line, limits.RecordBytes)
			}
			body.WriteByte('\n')
			body.WriteString(raw)
			rawRecord.WriteByte('\n')
			rawRecord.WriteString(raw)
			row.EndLine = line
			continue
		}
		if (match[1] == "[") != (match[4] == "] ") {
			return nil, "", fmt.Errorf("line %d: mismatched header delimiter", line)
		}
		if len(rows) >= limits.Records {
			return nil, "", fmt.Errorf("line %d: TXT exceeds %d records", line, limits.Records)
		}
		if len(raw) > limits.RecordBytes {
			return nil, "", fmt.Errorf("line %d: record exceeds %d bytes", line, limits.RecordBytes)
		}
		wall, precision, err := textWallTime(match[2], match[3], order)
		if err != nil {
			return nil, "", fmt.Errorf("line %d: %w", line, err)
		}
		instant, err := imazingcsv.ParseWallTime(wall, loc)
		if err != nil {
			return nil, "", fmt.Errorf("line %d: %w", line, err)
		}
		row := textRecord{Timestamp: match[2] + ", " + match[3], WallTime: wall, Precision: precision, Body: match[5], Kind: "notice", Raw: raw, StartLine: line, EndLine: line, SentAt: instant}
		if sender, body, ok := strings.Cut(match[5], ": "); ok && sender != "" && !textNotice.MatchString(sender) {
			row.Sender = sender
			row.Body = body
			row.Kind = "message"
		}
		flush()
		body.Reset()
		rawRecord.Reset()
		body.WriteString(row.Body)
		rawRecord.WriteString(row.Raw)
		rows = append(rows, row)
	}
	if limited.N == 0 {
		return nil, "", fmt.Errorf("TXT exceeds %d byte limit", limits.Bytes)
	}
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("line %d: read record (limit %d bytes): %w", line+1, limits.RecordBytes, err)
	}
	if len(rows) == 0 {
		return nil, "", errors.New("TXT contains no records")
	}
	flush()
	return rows, hex.EncodeToString(hash.Sum(nil)), nil
}

func textWallTime(date, clock, order string) (string, string, error) {
	sep := strings.IndexAny(date, "/.-")
	delimiter := date[sep : sep+1]
	parts := strings.Split(date, delimiter)
	if len(parts) != 3 {
		return "", "", fmt.Errorf("invalid date %q", date)
	}
	values := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return "", "", fmt.Errorf("invalid date %q", date)
		}
		values[i] = n
	}
	y, m, d := values[2], values[1], values[0]
	yearPart := parts[2]
	if order == "mdy" {
		m, d = values[0], values[1]
	}
	if order == "ymd" {
		y, m, d = values[0], values[1], values[2]
		yearPart = parts[0]
	}
	if len(yearPart) == 2 {
		if y >= 69 {
			y += 1900
		} else {
			y += 2000
		}
	} else if len(yearPart) != 4 {
		return "", "", errors.New("year must have two or four digits")
	}
	normalized := strings.ToUpper(strings.NewReplacer("\u202f", " ", "\u00a0", " ", ".", "").Replace(clock))
	fields := strings.Fields(normalized)
	c := strings.Split(fields[0], ":")
	h, _ := strconv.Atoi(c[0])
	minute, _ := strconv.Atoi(c[1])
	second := 0
	precision := "minute"
	if len(c) == 3 {
		second, _ = strconv.Atoi(c[2])
		precision = "second"
	}
	if len(fields) > 1 {
		if h < 1 || h > 12 {
			return "", "", fmt.Errorf("invalid 12-hour clock %q", clock)
		}
		h %= 12
		if strings.Join(fields[1:], "") == "PM" {
			h += 12
		}
	}
	wall := fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", y, m, d, h, minute, second)
	return wall, precision, nil
}
