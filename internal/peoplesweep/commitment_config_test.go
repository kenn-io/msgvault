package peoplesweep

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommitmentOutputLimitMatchesRunner(t *testing.T) {
	var cfg Config
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	cfg.Budgets.MaxOutputTokensPerPerson = 40000
	cfg.Commitments.MaxOutputTokens = 32768
	require.NoError(t, cfg.Commitments.Validate(cfg.Budgets))
	cfg.Commitments.MaxOutputTokens = 32769
	require.ErrorContains(t, cfg.Commitments.Validate(cfg.Budgets), "max_output_tokens <= 32768")
}
