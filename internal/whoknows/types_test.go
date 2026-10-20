package whoknows

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolve(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	p, err := Request{Topic: "  kubernetes networking "}.Resolve()
	require.NoError(err)
	assert.Equal(Params{
		Topic: "kubernetes networking", Terms: []string{"kubernetes", "networking"},
		FreeText: "kubernetes networking", Limit: 10, Evidence: 3, MinScore: .5,
	}, p)
	for _, topic := range []string{"C++ templates", "what is a pod?", "\"exact phrase\" notes"} {
		_, err := Request{Topic: topic}.Resolve()
		require.NoError(err, topic)
	}

	zero, high, early, late := 0, 51, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	for name, request := range map[string]Request{
		"limit":            {Topic: "x", Limit: &high},
		"evidence":         {Topic: "x", Evidence: &zero},
		"after not before": {Topic: "x", After: &late, Before: &early},
		"sender operator":  {Topic: "from:ana networking"},
		"date operator":    {Topic: "networking after:2026-01-01"},
	} {
		_, err := request.Resolve()
		require.ErrorIs(err, ErrInvalidParameter, name)
	}
	_, err = Request{Topic: "   "}.Resolve()
	require.ErrorIs(err, ErrMissingFreeText)
}
