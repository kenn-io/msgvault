package discord

import (
	"context"
	"encoding/csv"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/textutil"
)

const packageRecordLimit = 1 << 20

// packageRecord is shared by current JSON and legacy CSV transcripts.
type packageRecord struct {
	ID            string
	Timestamp     string
	Contents      string
	Attachments   []string
	Raw           []byte
	Format        string
	Path          string
	Ordinal       int
	InvalidReason string
}

type packageReader struct {
	root            *os.Root
	owner           string
	messages        string
	channels        []string
	channelsSkipped int
	titles          map[string]string
}

func openPackage(ctx context.Context, directory string) (*packageReader, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	r := &packageReader{root: root}
	fail := func(err error) (*packageReader, error) { _ = root.Close(); return nil, err }
	var account []byte
	for _, name := range []string{"account/user.json", "Account/user.json"} {
		account, err = r.readMetadata(ctx, name)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
	}
	if err != nil {
		return fail(fmt.Errorf("discord package account: %w", err))
	}
	var user struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(account, &user, json.MatchCaseInsensitiveNames(true)); err != nil {
		return fail(err)
	}
	id, err := strconv.ParseUint(user.ID, 10, 64)
	if err != nil || id == 0 {
		return fail(errors.New("discord package requires a numeric account ID in user.json"))
	}
	r.owner = user.ID
	var dir *os.File
	for _, name := range []string{"messages", "Messages"} {
		dir, err = root.Open(name)
		if err == nil {
			r.messages = name
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
	}
	if err != nil {
		return fail(err)
	}
	defer func() { _ = dir.Close() }()
	for {
		if err = ctx.Err(); err != nil {
			return fail(err)
		}
		entries, readErr := dir.ReadDir(256)
		for _, entry := range entries {
			// Never follow a directory symlink, including one aimed outside the root.
			if entry.Type()&os.ModeSymlink != 0 {
				r.channelsSkipped++
				continue
			}
			if !entry.IsDir() {
				continue
			}
			name := strings.TrimPrefix(entry.Name(), "c")
			if _, err := strconv.ParseUint(name, 10, 64); err != nil {
				r.channelsSkipped++
				continue
			}
			r.channels = append(r.channels, entry.Name())
		}
		if len(r.channels) > 100000 {
			return fail(errors.New("discord package has too many channels"))
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fail(readErr)
		}
	}
	sort.Strings(r.channels)
	index, err := r.readMetadata(ctx, path.Join(r.messages, "index.json"))
	if err == nil {
		if err = json.Unmarshal(index, &r.titles); err != nil {
			return fail(fmt.Errorf("discord package index: %w", err))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	return r, nil
}

func (r *packageReader) readMetadata(ctx context.Context, name string) ([]byte, error) {
	f, err := r.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(&export.ContextReader{Context: ctx, Reader: f}, packageRecordLimit+1))
	if err != nil {
		return nil, err
	}
	if len(b) > packageRecordLimit {
		return nil, fmt.Errorf("discord package metadata exceeds %d bytes: %s", packageRecordLimit, name)
	}
	return b, nil
}

func (r *packageReader) transcript(ctx context.Context, channel string, visit func(packageRecord) error) error {
	base := path.Join(r.messages, channel)
	for _, format := range []string{"json", "csv"} {
		name := path.Join(base, "messages."+format)
		f, err := r.root.Open(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		// Reset the read budget for each record. Decoder read-ahead is bounded by
		// the same budget; no transcript is loaded into memory as a whole.
		limited := &io.LimitedReader{R: &export.ContextReader{Context: ctx, Reader: f}, N: packageRecordLimit + 1}
		if format == "json" {
			err = readPackageJSON(limited, name, visit)
		} else {
			err = readPackageCSV(ctx, f, limited, name, visit)
		}
		return errors.Join(err, f.Close())
	}
	return fmt.Errorf("discord package channel %s has no messages.json or messages.csv", channel)
}

func readPackageJSON(input *io.LimitedReader, name string, visit func(packageRecord) error) error {
	dec := jsontext.NewDecoder(input)
	token, err := dec.ReadToken()
	if err != nil {
		return err
	}
	if token.Kind() != '[' {
		return errors.New("discord package transcript must be a JSON array")
	}
	for ordinal := 1; dec.PeekKind() != ']'; ordinal++ {
		input.N = packageRecordLimit + 1
		raw, err := dec.ReadValue()
		if err != nil {
			if input.N == 0 {
				return packageRecordLimitError(name, ordinal)
			}
			return fmt.Errorf("discord package %s record %d: %w", name, ordinal, err)
		}
		if len(raw) > packageRecordLimit {
			return packageRecordLimitError(name, ordinal)
		}
		record := packageRecord{Raw: append([]byte(nil), raw...), Format: "json", Path: name, Ordinal: ordinal}
		var fields struct {
			ID          jsontext.Value `json:"ID"`
			Timestamp   string         `json:"Timestamp"`
			Contents    string         `json:"Contents"`
			Attachments string         `json:"Attachments"`
		}
		if err = json.Unmarshal(raw, &fields, json.MatchCaseInsensitiveNames(true)); err != nil {
			record.InvalidReason = "incompatible JSON record"
			if semantic, ok := errors.AsType[*json.SemanticError](err); ok {
				record.InvalidReason = fmt.Sprintf("field %s: JSON %s cannot decode as %v", textutil.PrefixRunes(string(semantic.JSONPointer), 128), semantic.JSONKind, semantic.GoType)
			}
			// Decode the ID independently so a later field error cannot hide it.
			var identity struct {
				ID jsontext.Value `json:"ID"`
			}
			if json.Unmarshal(raw, &identity, json.MatchCaseInsensitiveNames(true)) == nil {
				record.ID = packageScalarID(identity.ID)
			}
		} else {
			record.ID = packageScalarID(fields.ID)
			record.Timestamp = fields.Timestamp
			record.Contents = fields.Contents
			record.Attachments = strings.Fields(fields.Attachments)
		}
		if err = visit(record); err != nil {
			return err
		}
	}
	if _, err = dec.ReadToken(); err != nil {
		return err
	}
	if _, err = dec.ReadToken(); !errors.Is(err, io.EOF) {
		return errors.New("discord package transcript has trailing data")
	}
	return nil
}

func packageScalarID(raw []byte) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	if _, err := strconv.ParseUint(string(raw), 10, 64); err == nil {
		return string(raw)
	}
	return ""
}

func readPackageCSV(ctx context.Context, f *os.File, input *io.LimitedReader, name string, visit func(packageRecord) error) error {
	reader := csv.NewReader(input)
	header, err := reader.Read()
	if err != nil {
		return err
	}
	if !slices.Equal(header, []string{"ID", "Timestamp", "Contents", "Attachments"}) {
		return errors.New("unsupported Discord package CSV header")
	}
	for ordinal := 1; ; ordinal++ {
		input.N = packageRecordLimit + 1
		start := reader.InputOffset()
		fields, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if input.N == 0 {
				return packageRecordLimitError(name, ordinal)
			}
			return fmt.Errorf("discord package %s row %d: %w", name, ordinal, err)
		}
		size := reader.InputOffset() - start
		if size > packageRecordLimit {
			return packageRecordLimitError(name, ordinal)
		}
		raw, err := io.ReadAll(&export.ContextReader{Context: ctx, Reader: io.NewSectionReader(f, start, size)})
		if err != nil {
			return err
		}
		if err = visit(packageRecord{ID: fields[0], Timestamp: fields[1], Contents: fields[2], Attachments: strings.Fields(fields[3]), Raw: raw, Format: "csv", Path: name, Ordinal: ordinal}); err != nil {
			return err
		}
	}
}

func packageRecordLimitError(name string, ordinal int) error {
	return fmt.Errorf("discord package %s record %d exceeds %d bytes", name, ordinal, packageRecordLimit)
}
