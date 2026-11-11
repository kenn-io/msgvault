package api

import (
	"go.kenn.io/msgvault/internal/changefeed"
	"go.kenn.io/msgvault/internal/store"
)

const changesScanBudget = changefeed.ScanBudget

func scanChanges(page store.ChangedMessagePage, since store.ChangedMessagesCursor, f changesFilter, limit int) ([]store.ChangedMessage, store.ChangedMessagesCursor, bool) {
	return changefeed.Scan(page, since, f.shared(), limit)
}
