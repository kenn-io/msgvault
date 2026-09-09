package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCronCorpusMatchesBrowserParser pins the daemon side of the shared cron
// corpus. The Web UI's cron parser runs the same file in its own tests, so a
// schedule the daemon stores never shows as invalid in the browser and the
// browser never accepts what the daemon refuses.
func TestCronCorpusMatchesBrowserParser(t *testing.T) {
	requirements := require.New(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "settings", "cron-corpus.json"))
	requirements.NoError(err)
	var corpus struct {
		Valid   []string `json:"valid"`
		Invalid []string `json:"invalid"`
	}
	requirements.NoError(json.Unmarshal(raw, &corpus))
	requirements.NotEmpty(corpus.Valid)
	requirements.NotEmpty(corpus.Invalid)

	for _, expression := range corpus.Valid {
		requirements.NoError(ValidateCronExpr(expression), "%q must parse", expression)
	}
	for _, expression := range corpus.Invalid {
		requirements.Error(ValidateCronExpr(expression), "%q must be rejected", expression)
	}
}
