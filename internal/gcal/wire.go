package gcal

import (
	"encoding/json/jsontext"
)

// Unexported JSON wire types for the Calendar API v3, mapped to the exported
// domain types. Each maps via a toX() method so the rest of the package never
// sees raw JSON shapes.

type wireCalendarListEntry struct {
	ID              string `json:"id"`
	Summary         string `json:"summary"`
	SummaryOverride string `json:"summaryOverride"`
	Description     string `json:"description"`
	TimeZone        string `json:"timeZone"`
	AccessRole      string `json:"accessRole"`
	Primary         bool   `json:"primary"`
	Deleted         bool   `json:"deleted"`
}

func (w wireCalendarListEntry) toCalendar() Calendar {
	summary := w.Summary
	if w.SummaryOverride != "" {
		// A user-set override (e.g. a renamed subscribed calendar) is the
		// label the user actually sees; prefer it.
		summary = w.SummaryOverride
	}
	return Calendar{
		ID:          w.ID,
		Summary:     summary,
		Description: w.Description,
		TimeZone:    w.TimeZone,
		AccessRole:  w.AccessRole,
		Primary:     w.Primary,
		Deleted:     w.Deleted,
	}
}

type wireCalendarList struct {
	Items         []wireCalendarListEntry `json:"items"`
	NextPageToken string                  `json:"nextPageToken"`
}

type wireEvents struct {
	// Items are kept as raw JSON so each event's original bytes can be
	// preserved verbatim in Event.Raw for the archive.
	Items         []jsontext.Value `json:"items"`
	NextPageToken string           `json:"nextPageToken"`
	NextSyncToken string           `json:"nextSyncToken"`
	TimeZone      string           `json:"timeZone"`
}
