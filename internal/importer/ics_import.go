package importer

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/store"
)

// ImportICS archives a local calendar export under a stable caller-chosen source.
func ImportICS(ctx context.Context, st *store.Store, path, identifier string) (*store.ICalendarImportSummary, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil, errors.New("--identifier is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open ICS file: %w", err)
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, calevent.MaxICSBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read ICS file: %w", err)
	}
	if len(raw) > calevent.MaxICSBytes {
		return nil, errors.New("ICS file exceeds 32 MiB limit")
	}
	if err := validateICSImport(st, identifier, raw); err != nil {
		return nil, err
	}
	source, err := st.GetOrCreateSource("ics", identifier)
	if err != nil {
		return nil, fmt.Errorf("create ICS source: %w", err)
	}
	summary, err := st.PersistICalendarContext(ctx, source.ID, raw)
	if err != nil {
		return nil, err
	}
	return &summary, nil
}

func validateICSImport(st *store.Store, identifier string, raw []byte) error {
	zone, err := icsImportZone(st, identifier)
	if err != nil {
		return err
	}
	components, err := calevent.ParseICS(raw, "", zone)
	if err != nil {
		return err
	}
	if len(components) == 0 {
		return errors.New("no event components to import")
	}
	return nil
}

func icsImportZone(st *store.Store, identifier string) (string, error) {
	source, err := st.GetSourceByTypeAndIdentifier("ics", identifier)
	if errors.Is(err, store.ErrSourceNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read ICS source: %w", err)
	}
	var config struct {
		Zone string `json:"time_zone"`
	}
	if json.Unmarshal([]byte(source.SyncConfig.String), &config) != nil {
		//nolint:nilerr // Match the store's UTC default for unreadable source configuration.
		return "", nil
	}
	return config.Zone, nil
}
