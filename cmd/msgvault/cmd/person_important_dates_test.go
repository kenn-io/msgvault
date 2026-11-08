package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPreviewImportantDates(t *testing.T) {
	dates := []store.PersonDate{{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: new(2), Day: new(29)}, Envelope: store.ValueEnvelope{Source: store.ProvenanceUser}}}
	rows := previewImportantDates(dates, time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC), time.UTC, "february_28")
	require.Len(t, rows, 2)
	assert.Equal(t, "2027-02-28", rows[0].Occurrence)
	assert.Equal(t, "date_valid", rows[0].Status)
	assert.Equal(t, "no_eligible_date", rows[1].Status)
}
