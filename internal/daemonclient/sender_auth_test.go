package daemonclient

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestSenderAuthMissingDaemonMetadata(t *testing.T) {
	checks := assert.New(t)
	checks.Nil(senderAuthFromGenerated(nil))
	oldDaemon := cliMessageDetailFromGenerated(&generated.GetCLIMessageResponse{ID: 7})
	checks.Equal(int64(7), oldDaemon.ID)
	checks.Nil(oldDaemon.SenderAuth)
}

func TestSenderAuthHybridResponseConversion(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	receipt := &generated.Verdict{Kind: "email", Basis: "provider_header", Dmarc: new("pass"), Verified: true, VerifiedDomain: new("example.test")}
	result, err := cliHybridSearchFromGenerated(generated.HybridSearchResponse{Results: []generated.HybridSearchItem{{ID: 1, SenderAuth: receipt}}})
	must.NoError(err)
	must.Len(result.Results, 1)
	must.NotNil(result.Results[0].Message.SenderAuth)
	checks.True(result.Results[0].Message.SenderAuth.Verified)
	checks.Equal(senderauth.Pass, result.Results[0].Message.SenderAuth.DMARC)
}
