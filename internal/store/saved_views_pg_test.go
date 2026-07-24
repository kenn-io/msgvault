package store_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSavedViewsPostgreSQLSchemaUsesIdentityJSONBAndTimestamps(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(testDB, "postgres://") && !strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("PostgreSQL-only saved views schema assertion")
	}

	st := testutil.NewTestStore(t)
	rows, err := st.DB().Query(`
		SELECT column_name, data_type, is_identity
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'saved_views'
	`)
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()

	types := map[string]string{}
	identities := map[string]string{}
	for rows.Next() {
		var name, dataType, identity string
		requirements.NoError(rows.Scan(&name, &dataType, &identity))
		types[name] = dataType
		identities[name] = identity
	}
	requirements.NoError(rows.Err())
	assertions.Equal("jsonb", types["canonical_state"])
	assertions.Equal("timestamp with time zone", types["created_at"])
	assertions.Equal("timestamp with time zone", types["updated_at"])
	assertions.Equal("YES", identities["id"])
}
