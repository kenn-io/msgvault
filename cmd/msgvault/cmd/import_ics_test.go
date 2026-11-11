package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportICSCommandImportsFile(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := lifecycleTestConfig(t.TempDir())
	ctx := withStoreResolverConfig(t, cfg)
	path := filepath.Join(t.TempDir(), "example.ics")
	raw := []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:command@example.com\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Example command event\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	requirements.NoError(os.WriteFile(path, raw, 0600))
	cmd := newImportICSCommand()
	var out bytes.Buffer
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{path, "--identifier", "example-export"})
	requirements.NoError(cmd.Execute())
	assertions.Equal("Events: 1 updated, 0 not updated\n", out.String())
	out.Reset()
	cmd = newImportICSCommand()
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{path, "--identifier", "example-export"})
	requirements.NoError(cmd.Execute())
	assertions.Equal("Events: 0 updated, 1 not updated\n", out.String())
	st, err := store.Open(cfg.DatabaseDSN())
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	_, err = st.GetSourceByTypeAndIdentifier(t.Context(), "ics", "example-export")
	requirements.NoError(err)
}

func TestImportICSCommandReportsSkippedZoneEvent(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := lifecycleTestConfig(t.TempDir())
	ctx := withStoreResolverConfig(t, cfg)
	path := filepath.Join(t.TempDir(), "calendar.ics")
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTIMEZONE\r\nTZID:Custom/Test\r\nBEGIN:STANDARD\r\nDTSTART:20270101T000000\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0500\r\nRDATE:20270301T020000\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:invalid@example.com\r\nDTSTART;TZID=Custom/Test:20270302T120000\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:valid@example.com\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Valid event\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	requirements.NoError(os.WriteFile(path, []byte(raw), 0600))
	cmd := newImportICSCommand()
	var output bytes.Buffer
	cmd.SetContext(ctx)
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{path, "--identifier", "zone-export"})
	requirements.NoError(cmd.Execute())
	assertions.Contains(output.String(), "unsupported VTIMEZONE")
	st, err := store.Open(cfg.DatabaseDSN())
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal("Valid event", page.Occurrences[0].Subject)
}
