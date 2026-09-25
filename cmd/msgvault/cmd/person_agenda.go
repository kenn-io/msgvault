package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/textutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newPersonAgendaCommand() *cobra.Command {
	var jsonOutput bool
	agenda := &cobra.Command{
		Use:   "agenda",
		Short: "Manage a person's live Kata agenda",
	}
	agenda.PersistentFlags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")

	list := &cobra.Command{
		Use:   "list <person-id>",
		Short: "List open Kata items linked to a person",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, err := positivePersonCLIArg(cmd, args[0], personValue)
			if err != nil {
				return err
			}
			client, closeClient, err := openPersonAgendaClient(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			result, err := client.ListPersonAgenda(cmd.Context(), personID)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writePersonAgendaJSON(cmd, result)
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(writer, "LIST\tREF\tSTATE\tPRIORITY\tTITLE")
			for _, item := range result.Items {
				priority := "-"
				if item.Priority != nil {
					priority = strconv.FormatInt(*item.Priority, 10)
				}
				_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
					textutil.SanitizeTerminal(item.List), textutil.SanitizeTerminal(item.Ref),
					textutil.SanitizeTerminal(item.State), priority, textutil.SanitizeTerminal(item.Title))
			}
			if err := writer.Flush(); err != nil {
				return fmt.Errorf("write agenda: %w", err)
			}
			if result.Truncated {
				_, err = fmt.Fprintln(cmd.ErrOrStderr(), "More open tasks are linked to this person. View the full list in Kata.")
			}
			if err != nil {
				return fmt.Errorf("write agenda notice: %w", err)
			}
			return nil
		},
	}

	var createTitle, createBody, createList, idempotencyKey string
	var createPriority int64
	var createLabels []string
	create := &cobra.Command{
		Use:   "create <person-id>",
		Short: "Create and link a Kata item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, err := positivePersonCLIArg(cmd, args[0], personValue)
			if err != nil {
				return err
			}
			if strings.TrimSpace(createTitle) == "" {
				return usageErr(cmd, errors.New("--title is required"))
			}
			request := generated.PersonAgendaCreateRequest{Title: createTitle, Labels: createLabels}
			if cmd.Flags().Changed("body") {
				request.Body = &createBody
			}
			if cmd.Flags().Changed("list") {
				request.List = &createList
			}
			if cmd.Flags().Changed("priority") {
				if err := validPersonAgendaPriority(createPriority); err != nil {
					return usageErr(cmd, err)
				}
				request.Priority = &createPriority
			}
			client, closeClient, err := openPersonAgendaClient(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			key := strings.TrimSpace(idempotencyKey)
			if key == "" {
				id, err := uuid.NewRandom()
				if err != nil {
					return fmt.Errorf("generate retry key: %w", err)
				}
				key = id.String()
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Idempotency key for retries: %s\n", key); err != nil {
					return fmt.Errorf("write retry key: %w", err)
				}
			}
			item, err := client.CreatePersonAgendaItem(cmd.Context(), personID, key, request)
			if err != nil {
				return err
			}
			return writePersonAgendaItem(cmd, item, jsonOutput)
		},
	}
	create.Flags().StringVar(&createTitle, "title", "", "Task title")
	create.Flags().StringVar(&createBody, "body", "", "Task body")
	create.Flags().StringVar(&createList, "list", "", "Virtual list")
	create.Flags().Int64Var(&createPriority, "priority", 0, "Kata priority from 0 through 4")
	create.Flags().StringSliceVar(&createLabels, "label", nil, "Kata label (repeatable)")
	create.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Retry key (generated when omitted; reuse for retries)")

	var linkList string
	link := &cobra.Command{
		Use:   "link <person-id> <ref>",
		Short: "Link an existing Kata item to a person",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, ref, err := personAgendaTarget(cmd, args)
			if err != nil {
				return err
			}
			request := generated.PersonAgendaLinkRequest{Ref: ref}
			if cmd.Flags().Changed("list") {
				request.List = &linkList
			}
			client, closeClient, err := openPersonAgendaClient(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			item, err := client.LinkPersonAgendaItem(cmd.Context(), personID, request)
			if err != nil {
				return err
			}
			return writePersonAgendaItem(cmd, item, jsonOutput)
		},
	}
	link.Flags().StringVar(&linkList, "list", "", "Virtual list")

	var editList string
	edit := &cobra.Command{
		Use:   "edit <person-id> <ref>",
		Short: "Move a linked Kata item to another list",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, ref, err := personAgendaTarget(cmd, args)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("list") {
				return usageErr(cmd, errors.New("--list is required; edit task content in Kata"))
			}
			client, closeClient, err := openPersonAgendaClient(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			item, err := client.UpdatePersonAgendaItem(cmd.Context(), personID, ref, generated.PersonAgendaUpdateRequest{List: editList})
			if err != nil {
				return err
			}
			return writePersonAgendaItem(cmd, item, jsonOutput)
		},
	}
	edit.Flags().StringVar(&editList, "list", "", "Replacement virtual list")

	unlink := &cobra.Command{
		Use:   "unlink <person-id> <ref>",
		Short: "Unlink a Kata item without deleting it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, ref, err := personAgendaTarget(cmd, args)
			if err != nil {
				return err
			}
			client, closeClient, err := openPersonAgendaClient(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			item, err := client.UnlinkPersonAgendaItem(cmd.Context(), personID, ref)
			if err != nil {
				return err
			}
			return writePersonAgendaItem(cmd, item, jsonOutput)
		},
	}

	agenda.AddCommand(list, create, link, edit, unlink)
	return agenda
}

func personAgendaTarget(cmd *cobra.Command, args []string) (int64, string, error) {
	personID, err := positivePersonCLIArg(cmd, args[0], personValue)
	if err != nil {
		return 0, "", err
	}
	ref := strings.TrimSpace(args[1])
	if ref == "" {
		return 0, "", usageErr(cmd, errors.New("kata ref must not be blank"))
	}
	return personID, ref, nil
}

func validPersonAgendaPriority(priority int64) error {
	if priority < 0 || priority > 4 {
		return errors.New("priority must be from 0 through 4")
	}
	return nil
}

func openPersonAgendaClient(cmd *cobra.Command) (*daemonclient.Client, func(), error) {
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return nil, nil, err
	}
	return client, func() { _ = client.Close() }, nil
}

func writePersonAgendaJSON(cmd *cobra.Command, value any) error {
	return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), value, json.Deterministic(true))
}

func writePersonAgendaItem(cmd *cobra.Command, item generated.PersonAgendaItem, jsonOutput bool) error {
	if jsonOutput {
		return writePersonAgendaJSON(cmd, item)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", textutil.SanitizeTerminal(item.Ref), textutil.SanitizeTerminal(item.Title))
	if err != nil {
		return fmt.Errorf("write person agenda item: %w", err)
	}
	return nil
}
