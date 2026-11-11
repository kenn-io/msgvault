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
	raw = bytes.Replace(raw, []byte("Example command event"), []byte("Changed title"), 1)
	requirements.NoError(os.WriteFile(path, raw, 0600))
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

func TestImportICSCommandUnusedAndUnsupportedZones(t *testing.T) {
	zone := "BEGIN:VTIMEZONE\r\nTZID:Custom/Test\r\nBEGIN:STANDARD\r\nDTSTART:20270101T000000\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0500\r\nRDATE:20270301T020000\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n"
	valid := "BEGIN:VEVENT\r\nUID:valid@example.com\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Valid event\r\nEND:VEVENT\r\n"
	invalid := "BEGIN:VEVENT\r\nUID:invalid@example.com\r\nDTSTART;TZID=Custom/Test:20270302T120000\r\nEND:VEVENT\r\n"
	for _, tc := range []struct {
		name, events string
		wantValid    bool
		wantError    bool
	}{
		{"unused", valid, true, false},
		{"mixed", invalid + valid, true, true},
		{"referenced", invalid, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
			cfg := lifecycleTestConfig(t.TempDir())
			ctx := withStoreResolverConfig(t, cfg)
			path := filepath.Join(t.TempDir(), "calendar.ics")
			raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" + zone + tc.events + "END:VCALENDAR\r\n"
			require.NoError(t, os.WriteFile(path, []byte(raw), 0600))
			cmd := newImportICSCommand()
			var output bytes.Buffer
			cmd.SetContext(ctx)
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs([]string{path, "--identifier", "zone-export"})
			err := cmd.Execute()
			if tc.wantError {
				report := output.String()
				if err != nil {
					report += err.Error()
				}
				assert.Contains(t, report, "unsupported VTIMEZONE")
			} else {
				require.NoError(t, err)
				assert.NotContains(t, output.String(), "unsupported VTIMEZONE")
			}
			if !tc.wantValid {
				return
			}
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, st.Close()) })
			from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			require.NoError(t, err)
			require.Len(t, page.Occurrences, 1)
			assert.Equal(t, "Valid event", page.Occurrences[0].Subject)
		})
	}
}
