package personobs

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRender(t *testing.T) {
	assert.Equal(t, "alice runs with bob", Render("[person] runs with [OWNER]", "alice", "bob"))
	assert.Equal(t, "[OWNER] and bob", Render("[PERSON] and [OWNER]", "[OWNER]", "bob"))
}
