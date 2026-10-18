package daemonclient

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/whoknows"
	"net/http"
	"strconv"
	"testing"
)

func TestWhoKnowsForwardsExactOptionsAndResponse(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	p := whoknows.DefaultParams()
	p.Evidence = 0
	p.HalfLifeDays = 0
	p.MinScore = .123456789012345
	p.IncludeUnsaved = false
	b := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check := assert.New(t)
		check.Equal("/api/v1/people/who-knows", r.URL.Path)
		check.Equal("cloud networking", r.URL.Query().Get("q"))
		check.Equal("0", r.URL.Query().Get("evidence"))
		check.Equal("0", r.URL.Query().Get("half_life_days"))
		check.Equal("false", r.URL.Query().Get("include_unsaved"))
		score, err := strconv.ParseFloat(r.URL.Query().Get("min_score"), 64)
		check.NoError(err)
		check.InDelta(p.MinScore, score, 0)
		writePeopleBrowserJSON(t, w, http.StatusOK, `{"topic":"cloud networking","people":[],"unsaved_candidates":[],"limits_applied":[],"coverage":{"stale_reasons":["contact_state_missing:1"]},"retrieval":{"underfilled":true},"tiebreak":{"requested":"temperature","applied":"last_contact"}}`)
	}))
	got, err := b.FindWhoKnows(context.Background(), "cloud networking", p)
	must.NoError(err)
	must.NotNil(got)
	check.Equal("cloud networking", got.Topic)
	check.True(got.Retrieval.Underfilled)
	check.Equal([]string{"contact_state_missing:1"}, got.Coverage.StaleReasons)
	check.Equal("last_contact", got.Tiebreak.Applied)
}
