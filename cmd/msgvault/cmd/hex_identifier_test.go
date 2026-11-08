package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/hex"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestHexPaddedIdentifierRegistrationAndSync(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.toml")
	must.NoError(os.WriteFile(path, []byte(fmt.Sprintf("[[hex]]\nidentifier = \" recorder \"\naccount_email = \"you@example.com\"\nmeetings_dir = %q\nenabled = true\nschedule = \"*/30 * * * *\"\n", root)), 0600))
	cfg, err := config.Load(path, "")
	if err != nil {
		must.ErrorContains(err, "identifier")
		must.ErrorContains(err, "whitespace", "a rejected source must explain the invalid identity")
		return
	}
	cfg.Data.DataDir = t.TempDir()
	must.Len(cfg.Hex, 1)
	source := cfg.Hex[0]
	st := testutil.NewTestStore(t)
	registered, err := registerMeetingSource(io.Discard, st, hex.SourceType, source.Identifier, source.AccountEmail)
	must.NoError(err)
	summary, err := hex.NewImporter(st).Import(t.Context(), hexImportOptions(source))
	must.NoError(err, "a configured source accepted by registration must sync under the same identifier")
	check.Equal(registered.ID, summary.SourceID)
	check.Equal(strings.TrimSpace(source.Identifier), registered.Identifier)
	must.NotNil(cfg.GetHexSource("recorder"))
	scheduled := cfg.ScheduledHexSources()
	must.Len(scheduled, 1)
	check.Equal(registered.Identifier, hexImportOptions(scheduled[0]).Identifier)
	normalized, err := (hex.RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: source.Identifier, AccountEmail: source.AccountEmail}}).Normalize()
	must.NoError(err)
	check.Equal(registered.Identifier, normalized.Source.Identifier)
}
