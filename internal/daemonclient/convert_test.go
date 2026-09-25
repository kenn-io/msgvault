package daemonclient

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestCLIAccountsFromGeneratedPreservesRawSourceType(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	lastSync := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)
	oauthApp := "named-app"
	accounts := cliAccountsFromGenerated(&generated.ListCLIAccountsResponse{
		Accounts: []generated.CliAccountResponse{
			{
				ID:                 7,
				Email:              "legacy@example.com",
				Type:               "",
				DisplayName:        "Legacy",
				OauthApp:           &oauthApp,
				MessageCount:       12,
				SourceDeletedCount: 3,
				LastSync:           &lastSync,
			},
			{Email: "explicit@example.com", Type: "gmail"},
			{Email: "imap@example.com", Type: "imap"},
		},
	})

	require.Len(accounts, 3)
	assert.Empty(accounts[0].Type, "legacy source type")
	assert.Equal("gmail", accounts[1].Type, "explicit Gmail source type")
	assert.Equal("imap", accounts[2].Type, "named source type")
	assert.Equal(int64(7), accounts[0].ID)
	assert.Equal("legacy@example.com", accounts[0].Email)
	assert.Equal("Legacy", accounts[0].DisplayName)
	assert.Equal("named-app", accounts[0].OAuthApp)
	assert.Equal(int64(12), accounts[0].MessageCount)
	assert.Equal(int64(3), accounts[0].SourceDeletedCount)
	require.NotNil(accounts[0].LastSync)
	assert.Equal(lastSync, *accounts[0].LastSync)
}

func TestCLIDeleteDedupedExecuteBodyKeepsEmptyExpectedBatches(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	expectedTotal := int64(0)
	expectedBatchCount := int64(0)
	body := cliDeleteDedupedExecuteBodyFromRequest(CLIDeleteDedupedRequest{
		BatchIDs:           []string{"batch-a"},
		ExpectedTotal:      &expectedTotal,
		ExpectedBatchCount: &expectedBatchCount,
		ExpectedBatches:    []CLIDeleteDedupedBatch{},
	})

	data, err := json.Marshal(body)
	require.NoError(err, "marshal execute body")

	var decoded map[string]any
	require.NoError(json.Unmarshal(data, &decoded), "decode execute body")
	expectedBatches, ok := decoded["expected_batches"].([]any)
	require.True(ok, "expected_batches should be present as an array: %s", string(data))
	assert.Empty(expectedBatches, "expected_batches")
}
