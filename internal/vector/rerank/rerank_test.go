package rerank

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrder(t *testing.T) {
	assert := assert.New(t)
	order, err := Order([]float64{0.2, 0.9, 0.9, 0.1})
	require.NoError(t, err)
	assert.Equal([]int{1, 2, 0, 3}, order)

	values := []float64{0.2, 0.9, 0.9, 0.1}
	_, err = Order(values)
	require.NoError(t, err)
	assert.Equal([]float64{0.2, 0.9, 0.9, 0.1}, values)
	order, err = Order([]float64{})
	require.NoError(t, err)
	assert.Empty(order)
}

func TestOrderRejectsInvalidScores(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), -0.1, 1.1} {
		_, err := Order([]float64{value})
		assert.Error(t, err)
	}
}
