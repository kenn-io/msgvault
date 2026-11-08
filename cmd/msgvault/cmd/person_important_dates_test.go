package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPreviewImportantDates(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	dates := []store.PersonDate{{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: new(2), Day: new(29)}, Envelope: store.ValueEnvelope{Source: store.ProvenanceUser}}}
	rows := previewImportantDates(dates, time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC), time.UTC, "february_28")
	must.Len(rows, 2)
	checks.Equal("2027-02-28", rows[0].Occurrence)
	checks.Equal("date_valid", rows[0].Status)
	checks.Equal("no_eligible_date", rows[1].Status)
}
