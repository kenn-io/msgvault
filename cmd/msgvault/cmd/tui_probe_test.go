package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBubbleteaHasNoImportTimeTerminalProbe guards msgvault's clean CLI
// startup. Upstream bubbletea v1 ships tea_init.go, whose package init()
// probes the terminal (OSC-11 background query) in every process that
// links it — meaning every msgvault command, not just `tui`. go.mod
// replaces bubbletea with third_party/bubbletea, which removes that file;
// the tui command performs the equivalent warm-up right before the
// Program starts. If the replace directive is dropped or an update
// reintroduces tea_init.go, this fails.
func TestBubbleteaHasNoImportTimeTerminalProbe(t *testing.T) {
	out, err := exec.Command(
		"go", "list", "-m", "-f", "{{.Dir}}", "github.com/charmbracelet/bubbletea",
	).CombinedOutput()
	require.NoError(t, err, "go list -m failed:\n%s", out)

	dir := strings.TrimSpace(string(out))
	require.NotEmpty(t, dir, "no directory resolved for bubbletea")

	_, statErr := os.Stat(filepath.Join(dir, "tea_init.go"))
	assert.True(t, os.IsNotExist(statErr),
		"resolved bubbletea module %s contains tea_init.go, whose init() probes the terminal on every msgvault command; keep the go.mod replace pointing at the patched third_party/bubbletea", dir)
}
