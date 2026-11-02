// Package linkedin imports LinkedIn member data exports without network access.
package linkedin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// Each CSV is bounded independently, including decompressed ZIP entries.
const maxCSVBytes = 64 << 20

type csvFile struct {
	name string
	kind string
	open func() (io.ReadCloser, error)
}

type row map[string]string

func discover(root string) ([]csvFile, func() error, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, fmt.Errorf("open export: %w", err)
	}
	var archive fs.FS
	closeArchive := func() error { return nil }
	if info.IsDir() {
		archive = os.DirFS(root)
	} else {
		if !info.Mode().IsRegular() {
			return nil, nil, errors.New("export must be a regular ZIP file or an extracted folder")
		}
		z, err := zip.OpenReader(root)
		if err != nil {
			return nil, nil, errors.New("export must be an extracted folder or a valid ZIP")
		}
		archive = z
		closeArchive = z.Close
	}
	var files []csvFile
	err = fs.WalkDir(archive, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		kind := strings.ToLower(path.Base(name))
		if strings.EqualFold(path.Base(path.Dir(name)), "messages") && strings.HasSuffix(kind, ".csv") {
			kind = "messages.csv"
		}
		switch kind {
		case "messages.csv", "comments.csv", "shares.csv", "connections.csv":
			files = append(files, csvFile{name: name, kind: kind, open: func() (io.ReadCloser, error) { return archive.Open(name) }})
		}
		return nil
	})
	if err != nil {
		_ = closeArchive()
		return nil, nil, fmt.Errorf("scan export: %w", err)
	}
	if len(files) == 0 {
		_ = closeArchive()
		return nil, nil, errors.New("no supported CSV files found in export")
	}
	// Enrichment precedes messages; posts precede comments even in a ZIP whose
	// directory ordering differs, so replies can point to their archived root.
	priority := map[string]int{"connections.csv": 0, "shares.csv": 1, "messages.csv": 2, "comments.csv": 3}
	sort.Slice(files, func(i, j int) bool {
		a, b := priority[files[i].kind], priority[files[j].kind]
		if a != b {
			return a < b
		}
		return files[i].name < files[j].name
	})
	return files, closeArchive, nil
}

func readRows(ctx context.Context, file csvFile, visit func(row, int) error) error {
	reader, err := file.open()
	if err != nil {
		return fmt.Errorf("open CSV: %w", err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(reader, maxCSVBytes+1))
	if err != nil {
		return fmt.Errorf("read CSV: %w", err)
	}
	if len(data) > maxCSVBytes {
		return errors.New("CSV exceeds the 64 MiB size limit")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if file.kind == "connections.csv" {
		// Connections exports have a variable prose preamble. Find the actual
		// header by parsing each physical line, rather than assuming three lines.
		offset := 0
		found := false
		for _, line := range bytes.SplitAfter(data, []byte{'\n'}) {
			fields, e := csv.NewReader(bytes.NewReader(line)).Read()
			if e == nil && hasHeader(fields, "first name") && hasHeader(fields, "last name") {
				data = data[offset:]
				found = true
				break
			}
			offset += len(line)
		}
		if !found {
			return errors.New("connections CSV has no name header")
		}
	}
	if file.kind == "comments.csv" {
		if err := validateCSV(ctx, data); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			decoded := unescapeComments(data)
			if err := validateCSV(ctx, decoded); err == nil {
				data = decoded
			} else if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
		}
	}

	csvReader := csv.NewReader(bytes.NewReader(data))
	csvReader.FieldsPerRecord = -1
	headers, err := csvReader.Read()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return errors.New("invalid CSV header")
	}
	seen := make(map[string]bool)
	for i, h := range headers {
		h = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))
		if seen[h] && h != "" {
			return errors.New("duplicate CSV header")
		}
		seen[h] = true
		headers[i] = h
	}
	for record := 2; ; record++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		values, err := csvReader.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("invalid CSV at record %d", record)
		}
		if len(values) > len(headers) {
			return fmt.Errorf("too many CSV fields at record %d", record)
		}
		r := make(row, len(headers))
		for i, v := range values {
			r[headers[i]] = v
		}
		if err := visit(r, record); err != nil {
			return fmt.Errorf("record %d: %w", record, err)
		}
	}
}

func hasHeader(fields []string, name string) bool {
	for _, f := range fields {
		if strings.EqualFold(strings.TrimSpace(f), name) {
			return true
		}
	}
	return false
}

// validateCSV checks CSV structure without retaining rows. A backslash next to
// a valid closing or doubled CSV quote is literal text, not legacy escaping.
func validateCSV(ctx context.Context, data []byte) error {
	reader := csv.NewReader(bytes.NewReader(data))
	reader.FieldsPerRecord = -1
	width := -1
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fields, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("validate CSV structure: %w", err)
		}
		if width < 0 {
			width = len(fields)
		} else if len(fields) > width {
			return errors.New("CSV record has more fields than its header")
		}
	}
}

// Convert LinkedIn's backslash escapes inside quoted comment fields to CSV
// quoting. Standard doubled quotes and literal unknown escapes stay intact.
func unescapeComments(data []byte) []byte {
	var out bytes.Buffer
	quoted := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if c == '"' {
			if quoted && i+1 < len(data) && data[i+1] == '"' {
				out.WriteString("\"\"")
				i++
				continue
			}
			quoted = !quoted
		}
		if quoted && c == '\\' && i+1 < len(data) {
			switch data[i+1] {
			case '"':
				out.WriteString("\"\"")
				i++
				continue
			case '\\':
				out.WriteByte('\\')
				i++
				continue
			case 'n':
				out.WriteByte('\n')
				i++
				continue
			case 'r':
				out.WriteByte('\r')
				i++
				continue
			case 't':
				out.WriteByte('\t')
				i++
				continue
			}
		}
		out.WriteByte(c)
	}
	return out.Bytes()
}

func date(value string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05", time.RFC3339Nano, "2006-01-02"} {
		t, err := time.Parse(layout, strings.TrimSpace(value))
		if err == nil {
			// Do not let time.Parse silently assign zero offset to unknown zones.
			if layout == "2006-01-02 15:04:05 MST" && !strings.HasSuffix(strings.TrimSpace(value), " UTC") {
				continue
			}
			return t.UTC(), nil
		}
	}
	return time.Time{}, errors.New("missing or invalid date (expected UTC)")
}
