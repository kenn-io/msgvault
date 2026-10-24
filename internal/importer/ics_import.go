package importer

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/store"
)

// ImportICS archives a local calendar export under a stable caller-chosen source.
// A non-empty timeZone becomes the source's zone for floating times.
func ImportICS(ctx context.Context, st *store.Store, path, identifier, timeZone string) (*store.ICalendarImportSummary, error) {
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
	if timeZone != "" {
		if _, err := time.LoadLocation(timeZone); err != nil {
			return nil, fmt.Errorf("time zone: %w", err)
		}
	}
	if err := validateICSImport(st, identifier, raw, timeZone); err != nil {
		return nil, err
	}
	source, err := st.GetOrCreateSource("ics", identifier)
	if err != nil {
		return nil, fmt.Errorf("create ICS source: %w", err)
	}
	if timeZone != "" {
		config, err := json.Marshal(map[string]string{"time_zone": timeZone})
		if err != nil {
			return nil, fmt.Errorf("encode ICS source config: %w", err)
		}
		if err := st.UpdateSourceSyncConfig(source.ID, string(config)); err != nil {
			return nil, fmt.Errorf("set ICS source time zone: %w", err)
		}
	}
	summary, err := st.PersistICalendarContext(ctx, source.ID, raw)
	if err != nil {
		return nil, err
	}
	return &summary, nil
}

func validateICSImport(st *store.Store, identifier string, raw []byte, zone string) error {
	if zone == "" {
		var err error
		if zone, err = icsImportZone(st, identifier); err != nil {
			return err
		}
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
