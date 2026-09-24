package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/textutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newIdentityMatchesCommand() *cobra.Command {
	command := &cobra.Command{Use: "matches", Short: "Review suggested identity matches", Args: cobra.NoArgs}
	command.AddCommand(newIdentityMatchesListCommand())
	command.AddCommand(newIdentityMatchesShowCommand())
	command.AddCommand(newIdentityMatchesDecisionCommand("accept"))
	command.AddCommand(newIdentityMatchesDecisionCommand("reject"))
	return command
}

func newIdentityMatchesListCommand() *cobra.Command {
	var state string
	var limit, offset int64
	var jsonOutput bool
	command := &cobra.Command{
		Use: "list", Short: "List reviewable identity matches", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if state != "all" && state != "candidate" && state != "accepted" &&
				state != "rejected" && state != "conflict" {
				return usageErr(cmd, errors.New("--state must be candidate, accepted, rejected, conflict, or all"))
			}
			if limit < 1 || limit > 500 || offset < 0 {
				return usageErr(cmd, errors.New("--limit must be 1–500 and --offset must be nonnegative"))
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			response, err := client.ListIdentityMatches(cmd.Context(), state, limit, offset)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeIdentityMatchesJSON(cmd.OutOrStdout(), response)
			}
			return writeIdentityMatchesTable(cmd.OutOrStdout(), response.Candidates)
		},
	}
	command.Flags().StringVar(&state, "state", "candidate", "Candidate state or all")
	command.Flags().Int64Var(&limit, "limit", 100, "Maximum matches (1–500)")
	command.Flags().Int64Var(&offset, "offset", 0, "Zero-based page offset")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print the complete API response")
	return command
}

func newIdentityMatchesShowCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use: "show <id>", Short: "Inspect one identity match and its review token", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := positiveIdentityMatchID(args[0])
			if err != nil {
				return usageErr(cmd, err)
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			response, err := client.GetIdentityMatch(cmd.Context(), id)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeIdentityMatchesJSON(cmd.OutOrStdout(), response)
			}
			return writeIdentityMatchDetail(cmd.OutOrStdout(), response)
		},
	}
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print the complete API response")
	return command
}

func newIdentityMatchesDecisionCommand(action string) *cobra.Command {
	var token, notesFile string
	var notesStdin, jsonOutput bool
	command := &cobra.Command{
		Use: action + " <id>", Short: strings.ToUpper(action[:1]) + action[1:] + " an identity match",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := positiveIdentityMatchID(args[0])
			if err != nil {
				return usageErr(cmd, err)
			}
			if strings.TrimSpace(token) == "" {
				return usageErr(cmd, errors.New("--review-token is required; inspect the match first"))
			}
			if cmd.Flags().Changed("notes-file") && notesStdin {
				return usageErr(cmd, errors.New("--notes-file and --notes-stdin are mutually exclusive"))
			}
			notes, err := identityMatchesNotes(cmd, notesFile, notesStdin)
			if err != nil {
				return err
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			if action == "accept" {
				response, err := client.AcceptIdentityMatch(cmd.Context(), id, token, notes)
				if err != nil {
					return err
				}
				if jsonOutput {
					return writeIdentityMatchesJSON(cmd.OutOrStdout(), response)
				}
				return writeIdentityMatchDecision(cmd.OutOrStdout(), &response.Candidate,
					response.IdentityRevision, string(response.CacheState))
			}
			response, err := client.RejectIdentityMatch(cmd.Context(), id, token, notes)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeIdentityMatchesJSON(cmd.OutOrStdout(), response)
			}
			return writeIdentityMatchDecision(cmd.OutOrStdout(), &response.Candidate,
				response.IdentityRevision, string(response.CacheState))
		},
	}
	command.Flags().StringVar(&token, "review-token", "", "Token from the reviewed match")
	command.Flags().StringVar(&notesFile, "notes-file", "", "Read decision notes from a file")
	command.Flags().BoolVar(&notesStdin, "notes-stdin", false, "Read decision notes from standard input")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print the complete API response")
	return command
}

func positiveIdentityMatchID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("identity match ID must be a positive integer")
	}
	return id, nil
}

func identityMatchesNotes(cmd *cobra.Command, file string, stdin bool) (*string, error) {
	if !stdin && !cmd.Flags().Changed("notes-file") {
		return nil, nil //nolint:nilnil // No notes were requested; omit the optional API field.
	}
	reader := cmd.InOrStdin()
	if !stdin {
		opened, err := os.Open(file)
		if err != nil {
			return nil, fmt.Errorf("open notes file: %w", err)
		}
		defer func() { _ = opened.Close() }()
		reader = opened
	}
	data, err := io.ReadAll(io.LimitReader(reader, 1<<20+1))
	if err != nil {
		return nil, fmt.Errorf("read identity match notes: %w", err)
	}
	if len(data) > 1<<20 {
		return nil, errors.New("identity match notes exceed 1 MiB")
	}
	notes := strings.TrimSpace(string(data))
	return &notes, nil
}

func writeIdentityMatchesJSON(w io.Writer, value any) error {
	return json.MarshalEncode(jsontext.NewEncoder(w), value, json.Deterministic(true))
}

func writeIdentityMatchesTable(w io.Writer, candidates []generated.IdentityMatchCandidate) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tSTATE\tENDPOINTS\tBASIS\tEVIDENCE\tREVIEW TOKEN\tBLOCKER"); err != nil {
		return fmt.Errorf("write identity match header: %w", err)
	}
	for _, candidate := range candidates {
		if _, err := fmt.Fprintf(tw, "%d\t%s\t%s:%d → %s:%d\t%s\t%d\t%s\t%s\n",
			candidate.ID, candidate.State, candidate.LeftKind, candidate.LeftID,
			candidate.RightKind, candidate.RightID, candidate.Basis,
			len(candidate.Evidence), textutil.SanitizeTerminal(stringOrEmpty(candidate.ReviewToken)),
			textutil.SanitizeTerminal(stringOrEmpty(candidate.Blocker))); err != nil {
			return fmt.Errorf("write identity match row: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flush identity match table: %w", err)
	}
	return nil
}

func writeIdentityMatchDetail(w io.Writer, c *generated.IdentityMatchCandidate) error {
	if _, err := fmt.Fprintf(w, "Match %d: %s:%d → %s:%d\nState: %s\nBasis: %s\nEvidence: %d\nReview token: %s\n",
		c.ID, c.LeftKind, c.LeftID, c.RightKind, c.RightID, c.State, c.Basis,
		len(c.Evidence), textutil.SanitizeTerminal(stringOrEmpty(c.ReviewToken))); err != nil {
		return fmt.Errorf("write identity match detail: %w", err)
	}
	for _, evidence := range c.Evidence {
		if _, err := fmt.Fprintf(w, "  - %s (%s; %d archive source(s))",
			textutil.SanitizeTerminal(evidence.EvidenceKind), evidence.Source,
			len(evidence.SourceSupport)); err != nil {
			return fmt.Errorf("write identity match evidence: %w", err)
		}
		if evidence.Detail != nil && *evidence.Detail != "" {
			if _, err := fmt.Fprintf(w, ": %s", textutil.SanitizeTerminal(*evidence.Detail)); err != nil {
				return fmt.Errorf("write identity match evidence detail: %w", err)
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return fmt.Errorf("finish identity match evidence: %w", err)
		}
	}
	if c.Blocker != nil && *c.Blocker != "" {
		_, err := fmt.Fprintf(w, "Blocker: %s\n", textutil.SanitizeTerminal(*c.Blocker))
		if err != nil {
			return fmt.Errorf("write identity match blocker: %w", err)
		}
	}
	return nil
}

func writeIdentityMatchDecision(w io.Writer, c *generated.IdentityMatchCandidate, revision int64, cacheState string) error {
	next := "Inspect the resulting person and CardDAV publication."
	if c.ApplicationPending {
		next = "Read this match again after link recovery before checking the person."
	} else if c.State == "rejected" {
		next = "No identity link was created."
	}
	_, err := fmt.Fprintf(w, "Match %d: %s\nIdentity revision: %d\nCache: %s\nApplication pending: %t\nNext: %s\n",
		c.ID, c.State, revision, cacheState, c.ApplicationPending, next)
	if err != nil {
		return fmt.Errorf("write identity match decision: %w", err)
	}
	return nil
}

func stringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
