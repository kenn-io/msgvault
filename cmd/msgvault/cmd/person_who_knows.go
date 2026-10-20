package cmd

import (
	"cmp"
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

const (
	whoKnowsCitationWidth = 80
	// whoKnowsMinAPISchemaVersion is the first schema with GET /people/who-knows.
	whoKnowsMinAPISchemaVersion = "3.24.0"
)

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
			query := generated.FindPeopleWhoKnowQuery{Topic: strings.Join(args, " ")}
			flags := cmd.Flags()
			if flags.Changed("limit") {
				query.Limit = new(int64(limit))
			}
			if flags.Changed("evidence") {
				query.Evidence = new(int64(evidence))
			}
			if flags.Changed("min-score") {
				query.MinScore = new(float32(minScore))
			}
			for _, date := range []struct {
				flag, value string
				target      **string
			}{{"--after", after, &query.After}, {"--before", before, &query.Before}} {
				parsed, err := parseDocumentSearchDate(date.value)
				if err != nil {
					return usageErr(cmd, fmt.Errorf("%s %w", date.flag, err))
				}
				if parsed != nil {
					*date.target = new(parsed.Format(time.RFC3339))
				}
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			version, err := client.APISchemaVersion(cmd.Context())
			if err != nil {
				return fmt.Errorf("check daemon who-knows capability: %w", err)
			}
			if !daemonclient.APISchemaVersionAtLeast(version, whoKnowsMinAPISchemaVersion) {
				return fmt.Errorf("person who-knows requires daemon API schema %s or newer (daemon reports %q); upgrade the daemon",
					whoKnowsMinAPISchemaVersion, version)
			}
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.FindPeopleWhoKnowResp, error) {
					return api.FindPeopleWhoKnowWithResponse(cmd.Context(),
						&generated.FindPeopleWhoKnowRequestOptions{Query: &query})
				})
			if err != nil {
				return err
			}
			if response.JSON200 == nil || len(response.Body) == 0 {
				return errors.New("who-knows response was empty")
			}
			if jsonOutput {
				_, err = cmd.OutOrStdout().Write(response.Body)
				return err
			}
			return writeWhoKnows(cmd.OutOrStdout(), *response.JSON200)
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

func writeWhoKnows(w io.Writer, response generated.WhoKnowsResponse) error {
	var out strings.Builder
	renderWhoKnows(&out, response)
	if _, err := io.WriteString(w, out.String()); err != nil {
		return fmt.Errorf("write who-knows output: %w", err)
	}
	return nil
}

// whoKnowsLimitLines say how each limit left the search short.
var whoKnowsLimitLines = map[generated.WhoKnowsResponseLimits]string{
	generated.PoolFull: fmt.Sprintf("Searched the %d closest messages; more may match. Narrow with --after or --before.",
		whoknows.SearchPool),
	generated.DeclaredTruncated: "Declared topics were searched only in part.",
	generated.IndexScope:        "The vector index doesn't cover email or chat, so messages weren't searched.",
}

func renderWhoKnows(out *strings.Builder, response generated.WhoKnowsResponse) {
	safe := textutil.SanitizeTerminal
	switch {
	case len(response.People) > 0 || len(response.UnsavedContacts) > 0:
		renderWhoKnowsEntries(out, response)
	case len(response.Limits) > 0:
		fmt.Fprintf(out, "No one turned up for %q.\n", safe(response.Topic))
	default:
		fmt.Fprintf(out, "Nobody in your archive has written about %q.\n", safe(response.Topic))
	}
	for _, limit := range response.Limits {
		fmt.Fprintln(out, cmp.Or(whoKnowsLimitLines[limit], safe(string(limit))))
	}
}

func renderWhoKnowsEntries(out *strings.Builder, response generated.WhoKnowsResponse) {
	safe := textutil.SanitizeTerminal
	for _, p := range response.People {
		fmt.Fprintf(out, "#%d  %s  score %.2f (messages %.2f, declared %.2f)  %s",
			p.Rank, safe(whoKnowsText(p.DisplayName)), p.Score, p.MessageScore, p.DeclaredBoost, authoredLabel(p.AuthoredMessages))
		if len(p.AskMeAbout) > 0 {
			fmt.Fprintf(out, "  declared: %s", safe(strings.Join(p.AskMeAbout, ", ")))
		}
		fmt.Fprintln(out)
		renderWhoKnowsCitations(out, p.Citations)
	}
	if len(response.UnsavedContacts) > 0 {
		fmt.Fprintln(out, "Unsaved contacts")
	}
	for _, c := range response.UnsavedContacts {
		var participant int64
		if c.ParticipantID != nil {
			participant = *c.ParticipantID
		}
		fmt.Fprintf(out, "#%d  %s  score %.2f  %s  msgvault person promote %d\n",
			c.Rank, contactLabel(c), c.Score, authoredLabel(c.AuthoredMessages), participant)
		renderWhoKnowsCitations(out, c.Citations)
	}
}

func whoKnowsText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func authoredLabel(n int64) string {
	if n == 0 {
		return "declared, no messages"
	}
	return fmt.Sprintf("%d authored", n)
}

func contactLabel(c generated.WhoKnowsEntry) string {
	safe := textutil.SanitizeTerminal
	name, email := whoKnowsText(c.DisplayName), whoKnowsText(c.EmailAddress)
	if name == "" || email == "" {
		return safe(cmp.Or(name, email))
	}
	return safe(name) + " <" + safe(email) + ">"
}

func renderWhoKnowsCitations(out *strings.Builder, citations []generated.WhoKnowsCitation) {
	for _, c := range citations {
		date := "undated"
		if c.SentAt != nil {
			date = c.SentAt.UTC().Format(time.DateOnly)
		}
		text := textutil.TruncateRunes(textutil.SanitizeTerminal(whoKnowsText(c.Subject)), whoKnowsCitationWidth)
		fmt.Fprintf(out, "    %s  %s  %s  msgvault show-message %d\n",
			date, textutil.SanitizeTerminal(c.MessageType), text, c.MessageID)
	}
}
