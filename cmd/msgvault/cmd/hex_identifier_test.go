package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/hex"
	"go.kenn.io/msgvault/internal/meetingimport"
)

func TestHexIdentifierContractMatchesRemoteRegistration(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		valid    bool
	}{
		{"boundary", strings.Repeat("é", 128), true},
		{"over limit", strings.Repeat("é", 129), false},
		{"unicode control", "rec\u0085order", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf("[[hex]]\nidentifier = %q\naccount_email = \"you@example.com\"\n", tc.id)), 0600))
			_, configErr := config.Load(path, "")
			_, remoteErr := (hex.RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: tc.id, AccountEmail: "you@example.com"}}).Normalize()
			if tc.valid {
				require.NoError(t, configErr)
				require.NoError(t, remoteErr)
			} else {
				require.ErrorContains(t, configErr, "identifier")
				require.Error(t, remoteErr)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[[hex]]\nidentifier = \" recorder \"\naccount_email = \"you@example.com\"\n"), 0600))
	_, err := config.Load(path, "")
	require.ErrorContains(t, err, "whitespace")
}
