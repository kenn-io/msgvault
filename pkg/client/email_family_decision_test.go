package client_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestGeneratedDecisionPreviewAllowsOmittedActorRefAndFamilyRevision(t *testing.T) {
	input := generated.EmailFamilyDecisionInput{
		ActorKind:  "cli",
		Kind:       "handled",
		MessageIds: []int64{42},
		RequestKey: "0123456789abcdef0123456789abcdef",
	}

	require.NoError(t, input.Validate())
}
