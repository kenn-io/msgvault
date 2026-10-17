package eval

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestGradedNDCGAt(t *testing.T) {
	check := assert.New(t)
	gains := map[string]int{"author": 3, "declared": 2, "received": 1, "excluded": 0}
	check.InDelta(1, GradedNDCGAt([]string{"author", "declared", "received"}, gains, 5), 1e-12)
	check.InDelta(.6806060568, GradedNDCGAt([]string{"received", "declared", "author"}, gains, 3), 1e-9)
	check.Zero(GradedNDCGAt(nil, gains, 5))
	check.Zero(GradedNDCGAt([]string{"author"}, gains, 0))
	check.Less(GradedNDCGAt([]string{"author", "author", "author"}, gains, 3), 1.0)
}
