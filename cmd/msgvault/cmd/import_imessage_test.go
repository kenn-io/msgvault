package cmd

import (
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// TestImportIMessage_NoAutoDefaultIdentity pins the documented behavior: the
// apple_messages source uses identifier "local" and the spec explicitly excludes
// this ingest path from auto-default-identity. After source creation via
// resolveImessageSource, account_identities must remain empty.
func TestImportIMessage_NoAutoDefaultIdentity(t *testing.T) {
	require := requirepkg.New(t)
	// After a successful import, account_identities has zero rows for the
	// apple_messages source. The source identifier is "local"; we never
	// auto-write because there's no per-user identifier known at source
	// creation time. Spec § Auto-default-identity § "Ingest paths that do
	// not auto-write".
	s, err := store.Open(filepath.Join(t.TempDir(), "msgvault.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	src, err := resolveImessageSource(s)
	require.NoError(err, "resolveImessageSource")

	rows, err := s.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	assertpkg.Empty(t, rows, "expected no account_identities rows for apple_messages source")
}

func TestResolveImessageSource(t *testing.T) {
	tests := []struct {
		name           string
		seedSources    []struct{ sourceType, identifier string }
		wantIdentifier string
	}{
		{
			name:           "no existing sources — creates local",
			seedSources:    nil,
			wantIdentifier: "local",
		},
		{
			name: "only local exists — reuses local",
			seedSources: []struct{ sourceType, identifier string }{
				{"apple_messages", "local"},
			},
			wantIdentifier: "local",
		},
		{
			name: "only legacy exists — reuses legacy",
			seedSources: []struct{ sourceType, identifier string }{
				{"apple_messages", "+15551234567"},
			},
			wantIdentifier: "+15551234567",
		},
		{
			name: "both legacy and local — prefers legacy",
			seedSources: []struct{ sourceType, identifier string }{
				{"apple_messages", "local"},
				{"apple_messages", "+15551234567"},
			},
			wantIdentifier: "+15551234567",
		},
		{
			name: "multiple legacy — picks first non-local",
			seedSources: []struct{ sourceType, identifier string }{
				{"apple_messages", "local"},
				{"apple_messages", "alice@icloud.com"},
				{"apple_messages", "+15551234567"},
			},
			// ListSources returns sorted by identifier;
			// +15551234567 sorts before alice@icloud.com
			wantIdentifier: "+15551234567",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := requirepkg.New(t)
			s, err := store.Open(":memory:")
			require.NoError(err)
			defer func() { _ = s.Close() }()

			require.NoError(s.InitSchema())

			for _, seed := range tt.seedSources {
				_, err := s.GetOrCreateSource(
					seed.sourceType, seed.identifier,
				)
				require.NoError(err, "seed source %q", seed.identifier)
			}

			src, err := resolveImessageSource(s)
			require.NoError(err, "resolveImessageSource")
			assertpkg.Equal(t, tt.wantIdentifier, src.Identifier, "identifier")
		})
	}
}
