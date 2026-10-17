package cmd

import (
	"fmt"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/textutil"
	"go.kenn.io/msgvault/internal/whoknows"
	"io"
	"strconv"
	"strings"
	"time"
)

func writeWhoKnowsResponse(cmd *cobra.Command, response whoknows.Response) error {
	w := cmd.OutOrStdout()
	safe := textutil.SanitizeTerminal
	if len(response.People) == 0 && len(response.UnsavedCandidates) == 0 {
		if _, err := fmt.Fprintf(w, "No one in the archive has authored content about %q.\n", safe(response.Topic)); err != nil {
			return fmt.Errorf("write topic summary: %w", err)
		}
	}
	for _, p := range response.People {
		if _, err := fmt.Fprintf(w, "#%d  %.3f  %s (person %d)\n", p.Rank, p.Score, safe(p.DisplayName), p.PersonID); err != nil {
			return fmt.Errorf("write person heading: %w", err)
		}
		b := p.Breakdown
		if _, err := fmt.Fprintf(w, "  evidence %.3f (depth %.3f × breadth %.2f; %d authored, %d received, %d threads, %s) · declared %.2f [%s] · affinity %.3f\n", b.Evidence, b.Depth, b.Breadth, b.AuthoredMessages, b.ReceivedMessages, b.ThreadsAuthored, strings.Join(b.ChannelsAuthored, "+"), b.Declared, safe(strings.Join(b.AskMeAboutMatches, ", ")), b.Affinity); err != nil {
			return fmt.Errorf("write person score breakdown: %w", err)
		}
		if err := writeWhoKnowsEvidence(w, p.Evidence); err != nil {
			return fmt.Errorf("write person evidence: %w", err)
		}
	}
	if len(response.UnsavedCandidates) > 0 {
		if _, err := fmt.Fprintln(w, "Unsaved contacts"); err != nil {
			return fmt.Errorf("write unsaved heading: %w", err)
		}
		for _, p := range response.UnsavedCandidates {
			if _, err := fmt.Fprintf(w, "  %.3f  %s <%s> · %s\n", p.Score, safe(p.DisplayName), safe(p.EmailAddress), p.PromoteHint); err != nil {
				return fmt.Errorf("write unsaved contact: %w", err)
			}
			if err := writeWhoKnowsEvidence(w, p.Evidence); err != nil {
				return fmt.Errorf("write unsaved evidence: %w", err)
			}
		}
	}
	if len(response.Organizations) > 0 {
		if _, err := fmt.Fprintln(w, "Organizations"); err != nil {
			return fmt.Errorf("write organization heading: %w", err)
		}
		for _, o := range response.Organizations {
			if _, err := fmt.Fprintf(w, "  %.3f  %s\n", o.Score, safe(o.Name)); err != nil {
				return fmt.Errorf("write organization score: %w", err)
			}
		}
	}
	e := response.Excluded
	if _, err := fmt.Fprintf(w, "Excluded: %d message types, %d owner messages, %d owner people, %d automated senders, %d large rooms, %d unresolved messages\n", e.MessageTypes, e.OwnerMessages, e.OwnerPeople, e.AutomatedSenders, e.LargeRooms, e.UnresolvedMessages); err != nil {
		return fmt.Errorf("write exclusion counts: %w", err)
	}
	if response.Coverage.Stale {
		line := strings.Join(response.Coverage.StaleReasons, ", ")
		if response.Coverage.Messages.Pending > 0 {
			line = formatWhoKnowsCount(response.Coverage.Messages.Pending) + " messages not embedded yet; recent evidence may rank low."
		}
		if _, err := fmt.Fprintf(w, "Coverage: %s\n", line); err != nil {
			return fmt.Errorf("write coverage: %w", err)
		}
	}
	if response.Retrieval.Underfilled {
		if _, err := fmt.Fprintln(w, "Retrieval pool underfilled after filtering; increase --candidates or [vector.search].k_per_signal."); err != nil {
			return fmt.Errorf("write retrieval hint: %w", err)
		}
	}
	return nil
}
func writeWhoKnowsEvidence(w io.Writer, evidence []whoknows.Evidence) error {
	for _, e := range evidence {
		date := "unknown date"
		if e.SentAt != nil {
			date = e.SentAt.UTC().Format(time.DateOnly)
		}
		if _, err := fmt.Fprintf(w, "  %s  %s  %s  %.3f  %q (message %d)\n", date, textutil.SanitizeTerminal(e.MessageType), e.Role, e.Contribution, textutil.SanitizeTerminal(e.Subject), e.MessageID); err != nil {
			return fmt.Errorf("write topic evidence: %w", err)
		}
		if e.Excerpt != nil {
			if _, err := fmt.Fprintf(w, "    %s\n", textutil.SanitizeTerminal(e.Excerpt.Snippet)); err != nil {
				return fmt.Errorf("write topic evidence: %w", err)
			}
		}
	}
	return nil
}
func formatWhoKnowsCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
