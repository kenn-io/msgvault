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
		Long: `Rank saved people, then unsaved contacts, by how many messages they wrote
that match the topic by meaning and how recent those messages are. Each
entry cites its messages; open one with 'msgvault show-message <id>'.
Messages from you and from automated senders do not count. Needs the
vector index; 'msgvault setup status' shows whether it is on.

A message counts when its meaning similarity to the topic is at least
--min-score. Lower it for a topic worded unlike your messages.`,
		Example: `  msgvault person who-knows "kubernetes networking"
  msgvault person who-knows "tax filing" --after 2026-01-01 --json`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request := whoknows.Request{
				Topic:  strings.Join(args, " "),
				After:  after,
				Before: before,
			}
			flags := cmd.Flags()
			if flags.Changed("limit") {
				request.Limit = &limit
			}
			if flags.Changed("evidence") {
				request.Evidence = &evidence
			}
			if flags.Changed("min-score") {
				request.MinScore = &minScore
			}
			if _, err := request.Resolve(); err != nil {
				return usageErr(cmd, err)
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			body := generated.FindPeopleWhoKnowBody{Topic: request.Topic}
			body.Limit = int64Pointer(request.Limit)
			body.Evidence = int64Pointer(request.Evidence)
			body.MinScore = request.MinScore
			if after != "" {
				body.After = &after
			}
			if before != "" {
				body.Before = &before
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

func int64Pointer(v *int) *int64 {
	if v == nil {
		return nil
	}
	n := int64(*v)
	return &n
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
	if len(response.People) == 0 && len(response.UnsavedContacts) == 0 {
		fmt.Fprintf(out, "Nobody in your archive has written about %q.\n", safe(response.Topic))
		return
	}
	for _, p := range response.People {
		fmt.Fprintf(out, "#%d  %s  score %.2f  %d authored\n",
			p.Rank, safe(p.DisplayName), p.Score, p.AuthoredMessages)
		renderWhoKnowsCitations(out, p.Citations)
	}
	if len(response.UnsavedContacts) == 0 {
		return
	}
	fmt.Fprintln(out, "Unsaved contacts")
	for _, c := range response.UnsavedContacts {
		fmt.Fprintf(out, "#%d  %s <%s>  score %.2f  %d authored  msgvault person promote %d\n",
			c.Rank, safe(c.DisplayName), safe(c.EmailAddress), c.Score, c.AuthoredMessages, c.ParticipantID)
		renderWhoKnowsCitations(out, c.Citations)
	}
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
