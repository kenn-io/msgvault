package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/personcard"
	"go.kenn.io/msgvault/internal/store"
)

const personCardsMinAPISchemaVersion = "3.26.0"

type personCardCommandClient interface {
	GetPersonCard(ctx context.Context, ref string, opts personcard.Options) (personcard.PersonCardResult, error)
	APISchemaVersion(ctx context.Context) (string, error)
}
type personCardCommandDeps struct {
	open func(context.Context) (personCardCommandClient, func(), error)
}

func defaultPersonCardCommandDeps() personCardCommandDeps {
	return personCardCommandDeps{open: func(ctx context.Context) (personCardCommandClient, func(), error) {
		client, _, err := OpenHTTPStore(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("open daemon: %w", err)
		}
		return client, func() { _ = client.Close() }, nil
	}}
}

func newPersonCardCommand(deps personCardCommandDeps) *cobra.Command {
	var asOf, format, affiliation string
	var maxItems, maxBytes int
	var jsonOutput bool
	command := &cobra.Command{
		Use: "card <person-ref>", Short: "Prepare for a meeting or reply with a cited person card",
		Long: "Read an owner-only person card with last interaction, ranked cited items, meeting history, honest gaps, and freshness. Private notes and sensitive attributes are excluded. as_of evaluates current snapshots; it is not a historical snapshot.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validatePersonCardCLIRef(args[0]); err != nil {
				return usageErr(cmd, err)
			}
			opts := personcard.Options{MaxItems: maxItems, MaxBytes: maxBytes, Format: personcard.Format(format), Affiliation: affiliation}
			if jsonOutput {
				opts.Format = personcard.FormatJSON
			}
			if maxItems < 1 || maxItems > 10 {
				return usageErr(cmd, errors.New("--max-items must be between 1 and 10"))
			}
			if maxBytes < 2048 || maxBytes > 65536 {
				return usageErr(cmd, errors.New("--max-bytes must be between 2048 and 65536"))
			}
			if opts.Format != personcard.FormatJSON && opts.Format != personcard.FormatMarkdown {
				return usageErr(cmd, errors.New("--format must be json or markdown"))
			}
			if affiliation != "" && affiliation != "internal" && affiliation != "external" {
				return usageErr(cmd, errors.New("--affiliation must be internal or external"))
			}
			if asOf != "" {
				parsed, err := time.Parse(time.RFC3339, asOf)
				if err != nil {
					return usageErr(cmd, errors.New("--as-of must be an RFC3339 timestamp"))
				}
				opts.AsOf = parsed
			}
			if deps.open == nil {
				return errors.New("person card daemon client is unavailable")
			}
			client, cleanup, err := deps.open(cmd.Context())
			if err != nil {
				return err
			}
			if cleanup != nil {
				defer cleanup()
			}
			if client == nil {
				return errors.New("person card daemon client is unavailable")
			}
			version, err := client.APISchemaVersion(cmd.Context())
			if err != nil {
				return fmt.Errorf("check daemon person card capability: %w", err)
			}
			if !daemonclient.APISchemaVersionAtLeast(version, personCardsMinAPISchemaVersion) {
				return fmt.Errorf("person cards require daemon API schema %s or newer (daemon reports %q); upgrade the daemon", personCardsMinAPISchemaVersion, version)
			}
			result, err := client.GetPersonCard(cmd.Context(), args[0], opts)
			if err != nil {
				return err
			}
			if opts.Format == personcard.FormatMarkdown && result.Content != nil {
				if _, err := fmt.Fprint(cmd.OutOrStdout(), *result.Content); err != nil {
					return fmt.Errorf("write person card: %w", err)
				}
				return nil
			}
			return writeMeetingJSON(cmd, result)
		},
	}
	command.Flags().StringVar(&asOf, "as-of", "", "RFC3339 evaluation time (default collection time)")
	command.Flags().IntVar(&maxItems, "max-items", personcard.DefaultMaxItems, "Maximum ranked items (1-10)")
	command.Flags().IntVar(&maxBytes, "max-bytes", personcard.DefaultMaxBytes, "Content byte budget (2048-65536)")
	command.Flags().StringVar(&format, "format", "markdown", "Card format: json or markdown")
	command.Flags().StringVar(&affiliation, "affiliation", "", "Optional caller-computed label: internal or external")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print the result envelope as JSON (overrides --format)")
	return command
}

func validatePersonCardCLIRef(ref string) error {
	if emailRef, ok := strings.CutPrefix(ref, "email:"); ok {
		email := strings.ToLower(strings.TrimSpace(emailRef))
		address, err := mail.ParseAddress(email)
		if err != nil || len(email) > 320 || strings.Count(email, "@") != 1 || strings.ContainsAny(email, "\r\n\t ") || address.Name != "" || address.Address != email {
			return errors.New("invalid email person reference")
		}
		return nil
	}
	_, _, err := store.ParsePersonMeetingRef(ref)
	return err
}

func init() { personCmd.AddCommand(newPersonCardCommand(defaultPersonCardCommandDeps())) }
