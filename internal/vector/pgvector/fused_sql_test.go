//go:build pgvector

package pgvector

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPostgresFTSRankExpression_RecipientAndBodyWeightsMatch(t *testing.T) {
	assert.Equal(t,
		"ts_rank_cd(ARRAY[0.1, 0.1, 0.4, 1.0]::real[], m.search_fts, to_tsquery('simple', $1), 32)",
		postgresFTSRankExpression("m.search_fts", "$1"),
	)
}
