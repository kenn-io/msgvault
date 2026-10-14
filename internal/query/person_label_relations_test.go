package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPersonLabelExpressionsUseExplicitRelations(t *testing.T) {
	assertions := assert.New(t)

	bestName := sqlClusterBestNameExprFrom("visible_label_participants", "pbn.id = p.id")
	assertions.Contains(bestName, "FROM visible_label_participants pbn")
	assertions.NotContains(bestName, "FROM participants pbn")

	fallback := sqlPersonIdentifierFallbackExprFrom("p", "visible_identifiers")
	assertions.Contains(fallback, "FROM visible_identifiers pi")
	assertions.NotContains(fallback, "FROM participant_identifiers pi")
}
