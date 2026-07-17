package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func configSnippetFromHint(t *testing.T, hint string) string {
	t.Helper()
	var lines []string
	started := false
	for line := range strings.SplitSeq(hint, "\n") {
		if strings.HasPrefix(line, "  ") {
			started = true
			lines = append(lines, strings.TrimPrefix(line, "  "))
			continue
		}
		if started && strings.TrimSpace(line) != "" {
			break
		}
	}
	require.NotEmpty(t, lines, "configuration hint must contain an indented TOML snippet")
	return strings.Join(lines, "\n") + "\n"
}

func TestMeetingConfigurationHintsLoad(t *testing.T) {
	for _, tt := range []struct {
		name string
		hint string
	}{
		{name: "Granola", hint: granolaConfigHint},
		{name: "Circleback", hint: circlebackConfigHint},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(os.WriteFile(path, []byte(configSnippetFromHint(t, tt.hint)), 0600))

			cfg, err := config.Load(path, "")

			require.NoError(err)
			switch tt.name {
			case "Granola":
				require.Len(cfg.Granola, 1)
				assert.Equal("you@example.com", cfg.Granola[0].AccountEmail)
			case "Circleback":
				require.Len(cfg.Circleback, 1)
				assert.Equal("you@example.com", cfg.Circleback[0].AccountEmail)
			}
		})
	}
}
