package cmd

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
)

func (a *storeAPIAdapter) ListCalendarOccurrencesContext(ctx context.Context, from, to time.Time) (store.CalendarOccurrencePage, error) {
	return a.store.ListCalendarOccurrencesContext(ctx, from, to)
}

var _ api.CalendarEventStore = (*storeAPIAdapter)(nil)
