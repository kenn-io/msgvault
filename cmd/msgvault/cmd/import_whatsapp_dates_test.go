package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseWhatsAppDateWindow(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	after, before, err := parseWhatsAppDateWindow("2026-01-02", "2026-02-03")
	require.NoError(err)
	assert.Equal(time.Date(2026, 1, 2, 0, 0, 0, 0, time.Local), after)
	assert.Equal(time.Date(2026, 2, 3, 0, 0, 0, 0, time.Local), before)

	after, before, err = parseWhatsAppDateWindow("", "")
	require.NoError(err)
	assert.True(after.IsZero())
	assert.True(before.IsZero())

	_, _, err = parseWhatsAppDateWindow("01/02/2026", "")
	require.ErrorContains(err, "invalid --after date")
	_, _, err = parseWhatsAppDateWindow("", "tomorrow")
	require.ErrorContains(err, "invalid --before date")
	_, _, err = parseWhatsAppDateWindow("2026-02-03", "2026-02-03")
	require.ErrorContains(err, "must be earlier than --before")
	_, _, err = parseWhatsAppDateWindow("2026-02-03", "2026-01-02")
	require.ErrorContains(err, "must be earlier than --before")
}
