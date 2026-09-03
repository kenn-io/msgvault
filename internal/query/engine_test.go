package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSemanticMessageSearchSupportsFilter_RejectsListID(t *testing.T) {
	assert.False(t, SemanticMessageSearchSupportsFilter(MessageFilter{
		ListID: "<dev_1@example.test>",
	}), "the hybrid API does not transport a List-Id scope")
}
