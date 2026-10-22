package cmd

import (
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/whoknows"
	"strconv"
	"time"
)

type whoKnowsOptions struct {
	p                                whoknows.Params
	halfLife, after, before          string
	savedOnly, noOrganizations, json bool
}

func newWhoKnowsOptions(cmd *cobra.Command) *whoKnowsOptions {
	o := &whoKnowsOptions{p: whoknows.DefaultParams()}
	f := cmd.Flags()
	f.IntVar(&o.p.Limit, "limit", 10, "Saved people to return (1–50)")
	f.IntVar(&o.p.Evidence, "evidence", 3, "Citations per person (0–10)")
	f.IntVar(&o.p.Candidates, "candidates", 200, "Candidate messages (50–1000)")
	f.StringVar(&o.halfLife, "half-life", "180", "Decay half-life in days or a Go duration; 0 disables decay")
	f.Float64Var(&o.p.MinScore, "min-score", 0, "Message vector-score floor; 0 disables it")
	f.Float64Var(&o.p.MinProfileScore, "min-profile-score", .5, "Profile similarity threshold (0–1)")
	f.StringVar(&o.after, "after", "", "Inclusive YYYY-MM-DD or RFC3339 lower bound")
	f.StringVar(&o.before, "before", "", "Exclusive RFC3339 upper bound, or inclusive YYYY-MM-DD")
	f.BoolVar(&o.p.IncludeOwner, "include-owner", false, "Include your identities and authored evidence")
	f.BoolVar(&o.savedOnly, "saved-only", false, "Omit unsaved contacts")
	f.BoolVar(&o.noOrganizations, "no-organizations", false, "Omit organization totals")
	f.StringVar(&o.p.Tiebreak, "tiebreak", "temperature", "temperature, last_contact, or none")
	f.BoolVar(&o.p.Excerpts, "excerpts", false, "Read one matching chunk for each citation")
	f.BoolVar(&o.json, "json", false, "Output the API JSON")
	return o
}
func (o *whoKnowsOptions) params() (whoknows.Params, error) {
	p := o.p
	var err error
	p.HalfLifeDays, err = parseWhoKnowsHalfLife(o.halfLife)
	if err != nil {
		return p, err
	}
	p.After, err = whoknows.ParseDate(o.after, false)
	if err != nil {
		return p, fmt.Errorf("--after: %w", err)
	}
	p.Before, err = whoknows.ParseDate(o.before, true)
	if err != nil {
		return p, fmt.Errorf("--before: %w", err)
	}
	p.IncludeUnsaved = !o.savedOnly
	p.IncludeOrganizations = !o.noOrganizations
	return p, p.Validate()
}
func parseWhoKnowsHalfLife(raw string) (int, error) {
	days, err := strconv.Atoi(raw)
	if err != nil {
		duration, parseErr := time.ParseDuration(raw)
		if parseErr != nil || duration < 0 {
			return 0, errors.New("--half-life must be days or a nonnegative duration")
		}
		days = int(duration / (24 * time.Hour))
		if duration%(24*time.Hour) != 0 {
			days++
		}
	}
	if days < 0 || days > 3650 {
		return 0, errors.New("--half-life must be between 0 and 3650 days")
	}
	return days, nil
}
