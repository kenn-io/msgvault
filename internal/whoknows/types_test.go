package whoknows

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveAppliesDefaults(t *testing.T) {
	p, err := Request{Topic: "  kubernetes networking "}.Resolve()
	require.NoError(t, err)
	assert.Equal(t, Params{
		Topic: "kubernetes networking", Terms: []string{"kubernetes", "networking"},
		FreeText: "kubernetes networking",
		Limit:    10, Evidence: 3, MinScore: .5,
	}, p)
}

func TestResolveKeepsTheDateWindow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	after, before := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	p, err := Request{Topic: "x", After: &after, Before: &before}.Resolve()
	require.NoError(err)
	assert.Equal(&after, p.After)
	assert.Equal(&before, p.Before)
}

func TestResolveRejectsInvalidRequests(t *testing.T) {
	intp := func(v int) *int { return &v }
	floatp := func(v float64) *float64 { return &v }
	early, late := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	for name, request := range map[string]Request{
		"limit low":        {Topic: "x", Limit: intp(0)},
		"limit high":       {Topic: "x", Limit: intp(51)},
		"evidence low":     {Topic: "x", Evidence: intp(0)},
		"evidence high":    {Topic: "x", Evidence: intp(11)},
		"min score zero":   {Topic: "x", MinScore: floatp(0)},
		"min score high":   {Topic: "x", MinScore: floatp(1.1)},
		"after not before": {Topic: "x", After: &late, Before: &early},
		"empty window":     {Topic: "x", After: &early, Before: &early},
		"sender operator":  {Topic: "from:ana networking"},
		"date operator":    {Topic: "networking after:2026-01-01"},
		"attachment":       {Topic: "invoice has:attachment"},
		"bad operator":     {Topic: "networking before:nonsense"},
	} {
		_, err := request.Resolve()
		require.ErrorIs(t, err, ErrInvalidParameter, name)
	}
	for _, topic := range []string{"   ", "\"\""} {
		_, err := Request{Topic: topic}.Resolve()
		require.ErrorIs(t, err, ErrMissingFreeText, topic)
	}
}

func TestResolveKeepsPlainWordsWithPunctuation(t *testing.T) {
	for _, topic := range []string{"C++ templates", "re: budget", "what is a pod?", "\"exact phrase\" notes"} {
		_, err := Request{Topic: topic}.Resolve()
		require.NoError(t, err, topic)
	}
}
