package identityindex

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentityRefreshPolicyExcludesColdActivity(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	root, db := writeRelationshipBaseFixture(t, false)
	_, err := db.Exec(`CREATE TEMP TABLE cold_messages AS SELECT *, 'cold'::VARCHAR AS conversation_tier FROM read_parquet(?)`, parquetDatasetGlob(root, "messages"))
	requirements.NoError(err)
	replaceRelationshipParquet(t, db, root, "messages", "SELECT * FROM cold_messages")
	result, err := Build(t.Context(), db, BuildOptions{Mode: ModeFull, StagedBaseRoot: root, OutputRoot: root})
	requirements.NoError(err)
	assertions.Equal(int64(0), result.Stats.TotalMessages)
	for _, dataset := range []string{DatasetActivity, DatasetPeople, DatasetDomains, DatasetRelationshipDaily, DatasetLogicalContributions, DatasetTemperatureContributions} {
		var count int64
		err := db.QueryRow(`SELECT count(*) FROM read_parquet(?)`, filepath.Join(root, dataset, "**", "*.parquet")).Scan(&count)
		requirements.NoError(err, dataset)
		assertions.Equal(int64(0), count, dataset)
	}
}
