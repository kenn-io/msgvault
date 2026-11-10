package reddit

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/textutil"
)

var contentName = regexp.MustCompile(`^(chat_history|messages_archive|messages|posts|comments)(_[0-9]+)?\.csv$`)
var statisticsName = regexp.MustCompile(`^statistics(_[0-9]+)?\.csv$`)

type csvFile struct{ name, kind string }
type csvRow struct {
	Filename string            `json:"filename"`
	Record   int               `json:"record"`
	Columns  map[string]string `json:"columns"`
}

type exportReader struct {
	root  *os.Root
	files []csvFile
	owner string
}

func displayHandle(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case len(s) >= 3 && strings.EqualFold(s[:3], "/u/"):
		s = s[3:]
	case len(s) >= 2 && strings.EqualFold(s[:2], "u/"):
		s = s[2:]
	}
	// Check original characters before case folding so Unicode cannot become ASCII.
	// Empty/bracketed markers and subreddit endpoints never identify a person.
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return ""
		}
	}
	return s
}

func openExport(ctx context.Context, dir, me string, summary *ImportSummary) (*exportReader, error) {
	owner := strings.ToLower(displayHandle(me))
	if me != "" && owner == "" {
		return nil, errors.New("reddit: invalid --me handle")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	r := &exportReader{root: root, owner: owner}
	fail := func(err error) (*exportReader, error) { _ = root.Close(); return nil, err }
	// Directory order must not decide which original wins an overlap; fs.ReadDir sorts by name.
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fail(err)
	}
	for _, entry := range entries {
		name := entry.Name()
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
		if stats {
			err = r.rows(ctx, csvFile{name, "statistics"}, nil, func(row csvRow) error {
				value := row.Columns["value"]
				switch row.Columns["statistic"] {
				case "account name":
					owner := strings.ToLower(displayHandle(value))
					if value != "" && owner == "" {
						return errors.New("invalid statistics account name")
					}
					if owner != "" {
						if r.owner != "" && owner != r.owner {
							return errors.New("reddit: account name conflicts with --me or statistics account")
						}
						r.owner = owner
					}
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
	reader := csv.NewReader(textutil.WithoutUTF8BOM(&export.ContextReader{Context: ctx, Reader: f}))
	headers, err := reader.Read()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("unsupported schema: %w", err)
	}
	reader.FieldsPerRecord = len(headers)
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
	required := map[string][]string{"chat": {"message_id", "message"}, "pm": {"id"}, "post": {"id"}, "comment": {"id"}, "statistics": {"statistic", "value"}}
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
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil && !errors.Is(err, csv.ErrFieldCount) {
			return fmt.Errorf("record %d: %w", number, err)
		}
		if summary != nil {
			summary.Rows++
		}
		for _, value := range values {
			if !utf8.ValidString(value) {
				return fmt.Errorf("record %d: invalid UTF-8 cell", number)
			}
		}
		if errors.Is(err, csv.ErrFieldCount) {
			if file.kind == "statistics" {
				return fmt.Errorf("record %d: %w", number, err)
			}
			if summary != nil {
				summary.Malformed++
			}
			continue
		}
		row := csvRow{Filename: file.name, Record: number, Columns: map[string]string{}}
		for i, v := range values {
			row.Columns[headers[i]] = v
		}
		if err := visit(row); err != nil {
			return fmt.Errorf("record %d: %w", number, err)
		}
	}
}
