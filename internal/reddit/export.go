package reddit

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/export"
)

var contentName = regexp.MustCompile(`^(chat_history|messages_archive|messages|posts|comments)(_[0-9]+)?\.csv$`)
var statisticsName = regexp.MustCompile(`^statistics(_[0-9]+)?\.csv$`)

type csvFile struct{ name, kind string }
type csvRow struct {
	Filename           string            `json:"filename"`
	Record             int               `json:"record"`
	Columns            map[string]string `json:"columns"`
	AccountEvidence    []string          `json:"account_evidence,omitempty"`
	ExportTimeEvidence []string          `json:"export_time_evidence,omitempty"`
}

type exportReader struct {
	root                  *os.Root
	files                 []csvFile
	owner                 string
	accounts, exportTimes []string
}

func normalizeHandle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.HasPrefix(s, "r/") || strings.HasPrefix(s, "/r/") {
		return ""
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "/u/"), "u/")
	switch s {
	case "", "[deleted]", "[removed]", "deleted", "unknown":
		return ""
	}
	return s
}

func openExport(ctx context.Context, dir, me string, summary *ImportSummary) (*exportReader, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	r := &exportReader{root: root, owner: normalizeHandle(me)}
	fail := func(err error) (*exportReader, error) { _ = root.Close(); return nil, err }
	d, err := root.Open(".")
	if err != nil {
		return fail(err)
	}
	entries, readErr := d.ReadDir(maxEntries + 1)
	closeErr := d.Close()
	if err = errors.Join(readErr, closeErr); err != nil && !errors.Is(err, io.EOF) {
		return fail(err)
	}
	if len(entries) > maxEntries {
		return fail(fmt.Errorf("reddit: export exceeds %d directory entries", maxEntries))
	}
	// Directory order must not decide which original wins an overlap.
	// ReadDir on File is unsorted; sort only the bounded inventory.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		info, err := root.Lstat(name)
		if err != nil {
			return fail(fmt.Errorf("reddit: %s: %w", name, err))
		}
		if !info.Mode().IsRegular() {
			summary.Unsupported = append(summary.Unsupported, name+" (nonregular)")
			continue
		}
		match := contentName.FindStringSubmatch(name)
		stats := statisticsName.MatchString(name)
		if match == nil && !stats {
			if strings.HasSuffix(strings.ToLower(name), ".csv") {
				summary.Unsupported = append(summary.Unsupported, name)
			}
			continue
		}
		if info.Size() > maxCSVBytes {
			return fail(fmt.Errorf("reddit: %s exceeds 256 MiB", name))
		}
		if stats {
			err = r.rows(ctx, csvFile{name, "statistics"}, nil, func(row csvRow) error {
				value := row.Columns["value"]
				switch row.Columns["statistic"] {
				case "account name":
					r.accounts = append(r.accounts, value)
					owner := normalizeHandle(value)
					if owner != "" {
						if r.owner != "" && owner != r.owner {
							return errors.New("reddit: account name conflicts with --me or statistics account")
						}
						r.owner = owner
					}
				case "export time":
					r.exportTimes = append(r.exportTimes, value)
				}
				return nil
			})
			if err != nil {
				return fail(err)
			}
		} else {
			kind := map[string]string{"chat_history": "chat", "messages": "pm", "messages_archive": "pm", "posts": "post", "comments": "comment"}[match[1]]
			r.files = append(r.files, csvFile{name, kind})
		}
	}
	if len(r.files) == 0 {
		return fail(errors.New("reddit: no recognized content CSV files"))
	}
	if r.owner == "" {
		return fail(errors.New("reddit: owner unavailable; supply --me or statistics.csv account name"))
	}
	return r, nil
}

func (r *exportReader) rows(ctx context.Context, file csvFile, summary *FileSummary, visit func(csvRow) error) (retErr error) {
	f, err := r.root.Open(file.name)
	if err != nil {
		return fmt.Errorf("reddit: %s: %w", file.name, err)
	}
	defer func() {
		retErr = errors.Join(retErr, f.Close())
		if retErr != nil {
			retErr = fmt.Errorf("reddit: %s: %w", file.name, retErr)
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("nonregular CSV")
	}
	if info.Size() > maxCSVBytes {
		return errors.New("CSV exceeds 256 MiB")
	}
	limited := &io.LimitedReader{R: &export.ContextReader{Context: ctx, Reader: f}, N: maxCSVBytes + 1}
	reader := csv.NewReader(limited)
	reader.FieldsPerRecord = -1
	headers, err := reader.Read()
	if err != nil {
		return fmt.Errorf("unsupported schema: %w", err)
	}
	headers[0] = strings.TrimPrefix(headers[0], "\ufeff")
	seen := map[string]bool{}
	for _, h := range headers {
		if !utf8.ValidString(h) {
			return errors.New("invalid UTF-8 header")
		}
		if h == "" || seen[h] {
			return errors.New("empty or duplicate header")
		}
		seen[h] = true
	}
	required := map[string][]string{"chat": {"message_id", "message"}, "pm": {"id", "body"}, "post": {"id", "title"}, "comment": {"id", "body"}, "statistics": {"statistic", "value"}}
	for _, key := range required[file.kind] {
		if !seen[key] {
			return fmt.Errorf("unsupported schema: missing essential column %s", key)
		}
	}
	for number := 1; ; number++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		values, err := reader.Read()
		if limited.N == 0 {
			return errors.New("CSV exceeds 256 MiB")
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if summary != nil {
			summary.Rows++
		}
		for _, value := range values {
			if !utf8.ValidString(value) {
				return errors.New("invalid UTF-8 cell")
			}
		}
		if len(values) != len(headers) {
			if summary != nil {
				summary.Malformed++
			}
			continue
		}
		row := csvRow{Filename: file.name, Record: number, Columns: map[string]string{}, AccountEvidence: r.accounts, ExportTimeEvidence: r.exportTimes}
		for i, v := range values {
			if !utf8.ValidString(v) {
				return errors.New("invalid UTF-8 cell")
			}
			row.Columns[headers[i]] = v
		}
		if err := visit(row); err != nil {
			return err
		}
	}
}
