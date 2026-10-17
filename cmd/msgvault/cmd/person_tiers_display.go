package cmd

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"go.kenn.io/msgvault/internal/textutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var personTierLaneOrder = []string{"tracking", "briefs", "enrichment"}

func personTierPrintf(w io.Writer, format string, values ...any) error {
	if _, err := fmt.Fprintf(w, format, values...); err != nil {
		return fmt.Errorf("write people tiers output: %w", err)
	}
	return nil
}

func personTierPlan(lanes map[string]string, deferred string) string {
	parts := []string{}
	for _, lane := range personTierLaneOrder {
		action := lanes[lane]
		switch action {
		case "", "unchanged":
			continue
		case "created":
			action = "create"
		case "removed":
			action = "remove"
		case "kept":
			action = "keep"
		case "blocked_manual":
			action = "blocked: manual"
		case "blocked_opt_out":
			action = "blocked: opt-out"
		}
		parts = append(parts, textutil.SanitizeTerminal(action)+" "+lane)
	}
	if deferred != "" {
		parts = append(parts, "deferred: "+textutil.SanitizeTerminal(strings.ReplaceAll(deferred, "_", " ")))
	}
	if len(parts) == 0 {
		return "unchanged"
	}
	return strings.Join(parts, "; ")
}

func personTierApplied(tier int64) int64 {
	if tier == 0 {
		return 3
	}
	return tier
}

func writePersonTierReadiness(w io.Writer, ready bool, reasons []string) error {
	detail := strings.Join(reasons, "; ")
	if detail == "" {
		detail = "none"
	}
	return personTierPrintf(w, "activity_ready: %t; reasons: %s\n", ready, textutil.SanitizeTerminal(detail))
}

func writePersonTierPage(w io.Writer, page generated.PersonTierPage) error {
	if err := personTierPrintf(w, "As of: %s\n", page.AsOf.Format(time.RFC3339)); err != nil {
		return err
	}
	if err := writePersonTierReadiness(w, page.Readiness.Ready, page.Readiness.Reasons); err != nil {
		return err
	}
	if err := writePersonTierCounts(w, page.Counts); err != nil {
		return err
	}
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if err := personTierPrintf(table, "PERSON\tNAME\tAPPLIED\tCOMPUTED\tRULE\tLANES\tPENDING DEMOTION\n"); err != nil {
		return err
	}
	for _, person := range page.People {
		due := person.Decision.DemotionDue
		if due == "" {
			due = "-"
		}
		if err := personTierPrintf(table, "%d\t%s\t%d\t%d\t%s\t%s\t%s\n", person.PersonID,
			textutil.SanitizeTerminal(person.Name), personTierApplied(person.Prior.Tier), person.Decision.ComputedTier,
			textutil.SanitizeTerminal(person.Decision.Rule), personTierPlan(person.Decision.Lanes, person.Decision.DeferredReason), textutil.SanitizeTerminal(due)); err != nil {
			return err
		}
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("write people tiers table: %w", err)
	}
	if page.NextCursor != "" {
		return personTierPrintf(w, "Next cursor: %s\n", textutil.SanitizeTerminal(page.NextCursor))
	}
	return nil
}

func writePersonTierExplanation(w io.Writer, explanation generated.PersonTierExplanation) error {
	d := explanation.Decision
	if err := personTierPrintf(w, "Person %d: %s\nAs of: %s\nApplied tier: %d\nComputed tier: %d\nRule: %s\nReason: %s\n",
		explanation.PersonID, textutil.SanitizeTerminal(explanation.Name), explanation.AsOf.Format(time.RFC3339),
		personTierApplied(explanation.Prior.Tier), d.ComputedTier, textutil.SanitizeTerminal(d.Rule), textutil.SanitizeTerminal(d.Reason)); err != nil {
		return err
	}
	if err := writePersonTierReadiness(w, explanation.Readiness.Ready, explanation.Readiness.Reasons); err != nil {
		return err
	}
	if err := personTierPrintf(w, "Lane plan: %s\nPending demotion: %s\n", personTierPlan(d.Lanes, d.DeferredReason), textutil.SanitizeTerminal(d.DemotionDue)); err != nil {
		return err
	}
	s := explanation.Signals
	for _, counter := range []struct {
		name  string
		value int64
	}{
		{"outbound_short", s.OutboundShort}, {"outbound_long", s.OutboundLong},
		{"inbound_short", s.InboundShort}, {"inbound_long", s.InboundLong},
		{"active_days_short", s.ActiveDaysShort}, {"active_days_long", s.ActiveDaysLong},
		{"meeting_days_long", s.MeetingDaysLong}, {"channels_long", s.ChannelsLong},
		{"excluded_list_events", s.ExcludedListEvents}, {"large_audience_inbound_events", s.LargeAudienceInboundEvents},
		{"co_presence_events", s.CoPresenceEvents}, {"observed_events", s.ObservedEvents},
	} {
		if err := personTierPrintf(w, "%s: %d\n", counter.name, counter.value); err != nil {
			return err
		}
	}
	lastDirect := "none"
	if s.LastDirectAt != nil {
		lastDirect = s.LastDirectAt.Format(time.RFC3339)
	}
	if err := personTierPrintf(w, "last_direct_at: %s\n", lastDirect); err != nil {
		return err
	}
	thresholds := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if err := personTierPrintf(thresholds, "THRESHOLD\tOBSERVED\tREQUIRED\tENABLED\tMET\n"); err != nil {
		return err
	}
	for _, threshold := range d.Thresholds {
		if err := personTierPrintf(thresholds, "%s\t%d\t%d\t%t\t%t\n", textutil.SanitizeTerminal(threshold.Name), threshold.Observed, threshold.Required, threshold.Enabled, threshold.Met); err != nil {
			return err
		}
	}
	if err := thresholds.Flush(); err != nil {
		return fmt.Errorf("write people tier thresholds: %w", err)
	}
	if tracking := explanation.Tracking; tracking.Tracked {
		provenance := tracking.Provenance
		if provenance == "" {
			provenance = "manual"
		}
		since := "unknown"
		if tracking.TrackedAt != nil {
			since = tracking.TrackedAt.Format(time.RFC3339)
		}
		if err := personTierPrintf(w, "tracking: provenance=%s actor=%s since=%s\n", textutil.SanitizeTerminal(provenance), personTierActor(tracking.Actor), since); err != nil {
			return err
		}
	} else if err := personTierPrintf(w, "tracking: none\n"); err != nil {
		return err
	}
	if brief := explanation.Briefs; brief.Enrolled {
		provenance := brief.Provenance
		if provenance == "" {
			provenance = "manual"
		}
		since := "unknown"
		if brief.EnabledAt != nil {
			since = brief.EnabledAt.Format(time.RFC3339)
		}
		if err := personTierPrintf(w, "briefs: provenance=%s actor=%s since=%s\n", textutil.SanitizeTerminal(provenance), personTierActor(brief.Actor), since); err != nil {
			return err
		}
	} else if err := personTierPrintf(w, "briefs: none\n"); err != nil {
		return err
	}
	if err := personTierPrintf(w, "Opt-outs:\n"); err != nil {
		return err
	}
	for _, lane := range personTierLaneOrder {
		for _, optOut := range explanation.OptOuts {
			if optOut.Lane != lane {
				continue
			}
			if err := personTierPrintf(w, "  %s actor=%s reason=%s created=%s\n", lane, personTierActor(optOut.Actor), textutil.SanitizeTerminal(optOut.Reason), optOut.CreatedAt.Format(time.RFC3339)); err != nil {
				return err
			}
		}
	}
	if len(explanation.OptOuts) == 0 {
		if err := personTierPrintf(w, "  none\n"); err != nil {
			return err
		}
	}
	if err := personTierPrintf(w, "Enrichment eligible: %t\nEnrichment consent: %t\nKnown suppression: %t\nSuppression: %s\nLinked participants: %d\nSignal note: %s\n",
		explanation.EnrichmentEligible, explanation.EnrichmentConsent, explanation.KnownSuppression,
		textutil.SanitizeTerminal(explanation.SuppressionNote), explanation.LinkedParticipants, textutil.SanitizeTerminal(explanation.SignalNote)); err != nil {
		return err
	}
	if explanation.Manual.OpenManualRunID != "" {
		if err := personTierPrintf(w, "Open manual enrichment run: %s\n", textutil.SanitizeTerminal(explanation.Manual.OpenManualRunID)); err != nil {
			return err
		}
	}
	if err := personTierPrintf(w, "Recent events (up to 5):\n"); err != nil {
		return err
	}
	events := explanation.RecentEvents
	if len(events) > 5 {
		events = events[:5]
	}
	return writePersonTierEvents(w, events)
}

func writePersonTierEvents(w io.Writer, events []generated.PersonTierEvent) error {
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if err := personTierPrintf(table, "EVENT\tPERSON\tWHEN\tFROM\tTO\tREASON\tMODE\tACTOR\tRUN\n"); err != nil {
		return err
	}
	for _, event := range events {
		from := "-"
		if event.FromTier != 0 {
			from = strconv.FormatInt(event.FromTier, 10)
		}
		if err := personTierPrintf(table, "%d\t%d\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", event.ID, event.PersonID,
			event.CreatedAt.Format(time.RFC3339), from, event.ToTier, textutil.SanitizeTerminal(event.Reason), textutil.SanitizeTerminal(event.Mode),
			personTierActor(event.Actor), textutil.SanitizeTerminal(event.RunID)); err != nil {
			return err
		}
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("write people tier history: %w", err)
	}
	return nil
}

func writePersonTierCandidates(w io.Writer, page generated.PersonTierCandidatePage) error {
	if err := personTierPrintf(w, "Unlinked candidates; basis: participant_approximation. Review identity matches before manual promotion.\n"); err != nil {
		return err
	}
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if err := personTierPrintf(table, "PARTICIPANT\tCANONICAL ID\tOUTBOUND\tINBOUND\tACTIVE DAYS\tMATCH CANDIDATE\tNEXT STEP\n"); err != nil {
		return err
	}
	for _, candidate := range page.Candidates {
		canonical, match := "-", "-"
		if candidate.CanonicalID != nil {
			canonical = textutil.SanitizeTerminal(*candidate.CanonicalID)
		}
		if candidate.IdentityMatchCandidateID != nil {
			match = strconv.FormatInt(*candidate.IdentityMatchCandidateID, 10)
		}
		if err := personTierPrintf(table, "%d\t%s\t%d\t%d\t%d\t%s\t%s\n", candidate.ParticipantID, canonical, candidate.OutboundShort, candidate.InboundShort,
			candidate.ActiveDaysShort, match, textutil.SanitizeTerminal(candidate.SuggestedCommand)); err != nil {
			return err
		}
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("write people tier candidates: %w", err)
	}
	if page.NextCursor != nil && *page.NextCursor != "" {
		return personTierPrintf(w, "Next cursor: %s\n", textutil.SanitizeTerminal(*page.NextCursor))
	}
	return nil
}
