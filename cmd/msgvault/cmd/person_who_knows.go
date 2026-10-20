package cmd

import (
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/textutil"
	"go.kenn.io/msgvault/internal/whoknows"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const whoKnowsCitationWidth = 80

func newPersonWhoKnowsCommand() *cobra.Command {
	var (
		limit, evidence int
		minScore        float64
		after, before   string
		jsonOutput      bool
	)
	command := &cobra.Command{
		Use:   "who-knows <topic words...>",
		Short: "Find the people who wrote about a topic",
		Long: `Rank saved people, then unsaved contacts, by how many recent messages they
wrote that match the topic by meaning. Each entry cites its messages; open one
with 'msgvault show-message <id>'. Messages from you and from automated senders
do not count. Needs the vector index. Lower --min-score for a topic worded
unlike your messages.`,
		Example: `  msgvault person who-knows "kubernetes networking"
  msgvault person who-knows "tax filing" --after 2026-01-01 --json`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := generated.FindPeopleWhoKnowBody{Topic: strings.Join(args, " ")}
			flags := cmd.Flags()
			if flags.Changed("limit") {
				body.Limit = new(int64(limit))
			}
			if flags.Changed("evidence") {
				body.Evidence = new(int64(evidence))
			}
			if flags.Changed("min-score") {
				body.MinScore = &minScore
			}
			var err error
			if body.After, err = parseDocumentSearchDate(after); err != nil {
				return usageErr(cmd, fmt.Errorf("--after %w", err))
			}
			if body.Before, err = parseDocumentSearchDate(before); err != nil {
				return usageErr(cmd, fmt.Errorf("--before %w", err))
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			if err := client.RequireWhoKnows(cmd.Context()); err != nil {
				return fmt.Errorf("who-knows: %w", err)
			}
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.FindPeopleWhoKnowResp, error) {
					return api.FindPeopleWhoKnowWithResponse(cmd.Context(),
						&generated.FindPeopleWhoKnowRequestOptions{Body: &body})
				})
			if err != nil {
				return err
			}
			if len(response.Body) == 0 {
				return errors.New("who-knows response was empty")
			}
			if jsonOutput {
				_, err = cmd.OutOrStdout().Write(response.Body)
				return err
			}
			var result whoknows.Response
			if err := json.Unmarshal(response.Body, &result); err != nil {
				return fmt.Errorf("decode who-knows response: %w", err)
			}
			return writeWhoKnows(cmd.OutOrStdout(), result)
		},
	}
	f := command.Flags()
	f.IntVar(&limit, "limit", whoknows.DefaultLimit, "Saved people to return (1-50)")
	f.IntVar(&evidence, "evidence", whoknows.DefaultEvidence, "Citations per person (1-10)")
	f.Float64Var(&minScore, "min-score", whoknows.DefaultMinScore, "Lowest meaning similarity a message needs to count (above 0, at most 1)")
	f.StringVar(&after, "after", "", "Only messages at or after this YYYY-MM-DD or RFC3339 time")
	f.StringVar(&before, "before", "", "Only messages before this YYYY-MM-DD or RFC3339 time")
	f.BoolVar(&jsonOutput, flagJSON, false, "Output the API JSON")
	return command
}

func writeWhoKnows(w io.Writer, response whoknows.Response) error {
	var out strings.Builder
	renderWhoKnows(&out, response)
	if _, err := io.WriteString(w, out.String()); err != nil {
		return fmt.Errorf("write who-knows output: %w", err)
	}
	return nil
}

func renderWhoKnows(out *strings.Builder, response whoknows.Response) {
	safe := textutil.SanitizeTerminal
	switch {
	case len(response.People) > 0 || len(response.UnsavedContacts) > 0:
		renderWhoKnowsPeople(out, response)
	case response.Reason == whoknows.ReasonSearchLimit:
		fmt.Fprintf(out, "No one turned up for %q within the search limits; narrow with --after or --before.\n", safe(response.Topic))
	default:
		fmt.Fprintf(out, "Nobody in your archive has written about %q.\n", safe(response.Topic))
	}
	if response.DeclaredTruncated {
		fmt.Fprintln(out, "Declared topics were searched only in part.")
	}
}

func renderWhoKnowsPeople(out *strings.Builder, response whoknows.Response) {
	safe := textutil.SanitizeTerminal
	for _, p := range response.People {
		fmt.Fprintf(out, "#%d  %s  score %.2f (messages %.2f, declared %.2f)  %s\n",
			p.Rank, safe(p.DisplayName), p.Score, p.MessageScore, p.DeclaredBoost, authoredLabel(p.AuthoredMessages))
		if len(p.AskMeAbout) > 0 {
			fmt.Fprintf(out, "    declared: %s\n", safe(strings.Join(p.AskMeAbout, ", ")))
		}
		renderWhoKnowsCitations(out, p.Citations)
	}
	if len(response.UnsavedContacts) == 0 {
		return
	}
	fmt.Fprintln(out, "Unsaved contacts")
	for _, c := range response.UnsavedContacts {
		fmt.Fprintf(out, "#%d  %s  score %.2f  %s  msgvault person promote %d\n",
			c.Rank, contactLabel(c), c.Score, authoredLabel(c.AuthoredMessages), c.ParticipantID)
		renderWhoKnowsCitations(out, c.Citations)
	}
}

func authoredLabel(n int) string {
	if n == 0 {
		return "declared, no messages"
	}
	return fmt.Sprintf("%d authored", n)
}

func contactLabel(c whoknows.Contact) string {
	safe := textutil.SanitizeTerminal
	if c.DisplayName == "" || c.EmailAddress == "" {
		return safe(cmp.Or(c.DisplayName, c.EmailAddress))
	}
	return safe(c.DisplayName) + " <" + safe(c.EmailAddress) + ">"
}

func renderWhoKnowsCitations(out *strings.Builder, citations []whoknows.Citation) {
	for _, c := range citations {
		date := "undated"
		if c.SentAt != nil {
			date = c.SentAt.UTC().Format(time.DateOnly)
		}
		text := textutil.TruncateRunes(textutil.SanitizeTerminal(cmp.Or(c.Subject, c.Snippet)), whoKnowsCitationWidth)
		fmt.Fprintf(out, "    %s  %s  %s  msgvault show-message %d\n",
			date, textutil.SanitizeTerminal(c.MessageType), text, c.MessageID)
	}
}
