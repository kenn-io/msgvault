package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assertions.Equal("Events: 1 updated, 0 unchanged\n", out.String())
}

func TestICalendarMovedEventInvalidatesCache(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	cfg, st := openTestDaemonAnalyticsStore(t)
	source, err := st.GetOrCreateSource("ics", "example-export")
	requirements.NoError(err)
	raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:moved@example.com\r\nSEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDTEND:20270302T130000Z\r\nSUMMARY:Meeting\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	_, err = st.PersistICalendarContext(t.Context(), source.ID, []byte(raw))
	requirements.NoError(err)
	_, err = buildCache(cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
	requirements.NoError(err)
	moved := strings.NewReplacer("SEQUENCE:1", "SEQUENCE:2", "20270302", "20270303").Replace(raw)
	_, err = st.PersistICalendarContext(t.Context(), source.ID, []byte(moved))
	requirements.NoError(err)
	stale, err := cacheNeedsBuildForServing(t.Context(), cfg.DatabaseDSN(), cfg.AnalyticsDir())
	requirements.NoError(err)
	assertions.True(stale.FullRebuild, stale.Reason)
}
