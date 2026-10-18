package cmd

import (
	"bytes"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/whoknows"
	"testing"
)

func TestWhoKnowsHalfLife(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want int
	}{{"0", 0}, {"180", 180}, {"12h", 1}, {"25h", 2}, {"24h1ns", 2}} {
		got, err := parseWhoKnowsHalfLife(tt.raw)
		require.NoError(t, err)
		assert.Equal(t, tt.want, got)
	}
	for _, raw := range []string{"-1", "-12h", "3651", "87600h1ns", "bad"} {
		_, err := parseWhoKnowsHalfLife(raw)
		require.Error(t, err)
	}
}
func TestWhoKnowsRenderEmptyAndCoverage(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cmd := &cobra.Command{}
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	response := whoknows.Response{Topic: "networking", People: []whoknows.Ranked{}, UnsavedCandidates: []whoknows.UnsavedCandidate{}, Coverage: whoknows.Coverage{Stale: true, Messages: whoknows.MessageCoverage{Pending: 1800}}}
	must.NoError(writeWhoKnowsResponse(cmd, response))
	check.Contains(buf.String(), "No one in the archive")
	check.Contains(buf.String(), "1,800 messages")
}

func TestWhoKnowsOptionFlags(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	command := &cobra.Command{}
	options := newWhoKnowsOptions(command)
	must.NoError(command.ParseFlags([]string{"--evidence=0", "--half-life=12h", "--saved-only", "--no-organizations", "--tiebreak=none", "--before=2026-10-17"}))
	p, err := options.params()
	must.NoError(err)
	check.Zero(p.Evidence)
	check.Equal(1, p.HalfLifeDays)
	check.False(p.IncludeUnsaved)
	check.False(p.IncludeOrganizations)
	check.Equal("none", p.Tiebreak)
}
