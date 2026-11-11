package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalendarInvalidTimeNamesValueAndFormat(t *testing.T) {
	_, err := parseCalendarBound("bad-time", "", false)
	require.Error(t, err)
	require.ErrorContains(t, err, "bad-time")
	require.ErrorContains(t, err, "2026-10-10T09:00:00-07:00")
}

func TestCalendarMissingAccountNamesTheFlag(t *testing.T) {
	command := newCalendarControlCmd(nil)
	command.SetArgs([]string{"freebusy", "primary"})
	err := command.Execute()
	require.Error(t, err)
	assert.ErrorContains(t, err, "--account")
}
