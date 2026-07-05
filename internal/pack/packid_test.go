package pack

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewPackID(t *testing.T) {
	assert := assert.New(t)
	seen := make(map[string]bool)
	prev := ""
	for range 1000 {
		id := NewPackID()
		assert.Len(id, 26)
		assert.Equal(strings.ToLower(id), id, "pack IDs must be lowercase")
		assert.True(IsValidPackID(id), "generated ID must validate: %s", id)
		assert.False(seen[id], "duplicate pack ID: %s", id)
		seen[id] = true
		if prev != "" {
			// ULIDs generated in sequence must sort ascending (time-ordered
			// with monotonic entropy within the same millisecond).
			assert.Less(prev, id)
		}
		prev = id
	}
}

func TestIsValidPackID(t *testing.T) {
	assert := assert.New(t)
	assert.False(IsValidPackID(""))
	assert.False(IsValidPackID("not-a-ulid"))
	assert.False(IsValidPackID(strings.ToUpper(NewPackID())), "uppercase is rejected")
	assert.False(IsValidPackID(NewPackID() + "x"))
}
