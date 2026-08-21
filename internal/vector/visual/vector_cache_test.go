package visual

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestQueryVectorCacheKeysAndEviction(t *testing.T) {
	assert := assert.New(t)
	var cache queryVectorCache
	vector := []float32{1, 2, 3}
	cache.put(1, "hash-a", vector)
	assert.Equal(vector, cache.get(1, "hash-a"))
	assert.Nil(cache.get(2, "hash-a"),
		"a generation swap must never reuse another generation's vector")
	assert.Nil(cache.get(1, "hash-b"))
	for i := range queryVectorCacheCap {
		cache.put(3, "filler-"+strconv.Itoa(i), vector)
	}
	assert.Nil(cache.get(1, "hash-a"), "the oldest entry is evicted at capacity")
}
