package cmd

import (
	"errors"
	"log/slog"

	"go.kenn.io/msgvault/internal/store"
)

func updateSourceDisplayNameForRegistration(
	st *store.Store,
	sourceID int64,
	displayName string,
	logger *slog.Logger,
) error {
	err := st.UpdateSourceDisplayName(sourceID, displayName)
	if !errors.Is(err, store.ErrSourceSettingsInvalid) {
		return err
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("source display name conflicts with another source selector; keeping current name",
		"source_id", sourceID,
	)
	return nil
}
