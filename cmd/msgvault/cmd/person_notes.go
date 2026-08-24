package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/store"
)

var (
	personNotesJSON       bool
	personNotesText       string
	personNotesExpectedID int64
)

var personNotesCmd = &cobra.Command{
	Use:   "notes",
	Short: "Read and update private person notes",
	Long: "Read and update private user-curated notes for a durable person profile. " +
		"Observed contacts must be promoted before notes can be written.",
}

var personNotesGetCmd = &cobra.Command{
	Use:   "get <person-id>",
	Short: "Get a person's notes",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		personID, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		backend, closeBackend, err := openPersonNotesBackend(cmd)
		if err != nil {
			return err
		}
		defer closeBackend()
		attributes, err := backend.ListAttributesByUniversalID(
			cmd.Context(), personID, store.AttributeUniversalIDNotes,
		)
		if err != nil {
			return err
		}
		group, value := personNotesAttribute(attributes)
		if group == nil {
			return errors.New("notes attribute definition is unavailable")
		}
		if personNotesJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
		}
		if value == nil {
			return nil
		}
		if value.Value.Text == nil {
			return errors.New("notes attribute response was not text")
		}
		if _, err := io.WriteString(cmd.OutOrStdout(), *value.Value.Text); err != nil {
			return fmt.Errorf("write person notes: %w", err)
		}
		if !strings.HasSuffix(*value.Value.Text, "\n") {
			_, err = fmt.Fprintln(cmd.OutOrStdout())
		}
		if err != nil {
			return fmt.Errorf("finish person notes output: %w", err)
		}
		return nil
	},
}

var personNotesSetCmd = &cobra.Command{
	Use:   "set <person-id>",
	Short: "Replace a person's notes",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		personID, text, err := personNotesWriteInput(cmd, args)
		if err != nil {
			return err
		}
		var expectedValueID *int64
		if cmd.Flags().Changed("expected-value-id") {
			if personNotesExpectedID < 1 {
				return usageErr(cmd, errors.New(
					"--expected-value-id must be a positive integer"))
			}
			expectedValueID = &personNotesExpectedID
		}
		backend, closeBackend, err := openPersonNotesBackend(cmd)
		if err != nil {
			return err
		}
		defer closeBackend()
		attributes, err := backend.ListAttributesByUniversalID(
			cmd.Context(), personID, store.AttributeUniversalIDNotes,
		)
		if err != nil {
			return err
		}
		group, _ := personNotesAttribute(attributes)
		if group == nil {
			return errors.New("notes attribute definition is unavailable")
		}
		write, err := backend.SetAttribute(cmd.Context(), peoplebrowser.SetAttributeRequest{
			PersonID: personID, Slug: group.Definition.Slug,
			Value:           store.AttributeValue{Type: store.AttributeValueText, Text: &text},
			ExpectedValueID: expectedValueID,
		})
		if err != nil {
			return err
		}
		return writePersonNotesResult(cmd, write)
	},
}

var personNotesAppendCmd = &cobra.Command{
	Use:   "append <person-id>",
	Short: "Atomically append to a person's notes",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		personID, text, err := personNotesWriteInput(cmd, args)
		if err != nil {
			return err
		}
		backend, closeBackend, err := openPersonNotesBackend(cmd)
		if err != nil {
			return err
		}
		defer closeBackend()
		write, err := backend.AppendNote(cmd.Context(), peoplebrowser.AppendNoteRequest{
			PersonID: personID, Text: text,
		})
		if err != nil {
			return err
		}
		return writePersonNotesResult(cmd, write)
	},
}

func openPersonNotesBackend(
	cmd *cobra.Command,
) (*daemonclient.PeopleBrowser, func(), error) {
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return nil, nil, err
	}
	return daemonclient.NewPeopleBrowser(daemonclient.NewEngineAdapter(client)),
		func() { _ = client.Close() }, nil
}

func personNotesWriteInput(cmd *cobra.Command, args []string) (int64, string, error) {
	personID, err := positivePersonCLIArg(cmd, args[0], personValue)
	if err != nil {
		return 0, "", err
	}
	if !cmd.Flags().Changed("text") {
		return 0, "", usageErr(cmd, errors.New("--text is required"))
	}
	if strings.TrimSpace(personNotesText) == "" {
		return 0, "", usageErr(cmd, errors.New("notes text must not be blank"))
	}
	document, err := readCLIDocument(cmd, personNotesText)
	if err != nil {
		return 0, "", err
	}
	text := string(document)
	if strings.TrimSpace(text) == "" {
		return 0, "", usageErr(cmd, errors.New("notes text must not be blank"))
	}
	return personID, text, nil
}

func personNotesAttribute(attributes *peoplebrowser.Attributes) (
	*peoplebrowser.AttributeGroup, *store.PersonAttributeValue,
) {
	if attributes == nil {
		return nil, nil
	}
	for i := range attributes.Groups {
		group := &attributes.Groups[i]
		if group.Definition.UniversalID != store.AttributeUniversalIDNotes {
			continue
		}
		if len(group.Current) == 0 {
			return group, nil
		}
		return group, &group.Current[0]
	}
	return nil, nil
}

func writePersonNotesResult(cmd *cobra.Command, write *store.PersonAttributeWrite) error {
	if write == nil {
		return errors.New("person notes response was empty")
	}
	if personNotesJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(write)
	}
	if write.Value == nil || write.Value.Value.Text == nil {
		return errors.New("person notes response contained no text value")
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "Updated notes value %d\n", write.Value.ID)
	if err != nil {
		return fmt.Errorf("write person notes result: %w", err)
	}
	return nil
}

func init() {
	personNotesCmd.AddCommand(personNotesGetCmd, personNotesSetCmd, personNotesAppendCmd)
	for _, command := range []*cobra.Command{
		personNotesGetCmd, personNotesSetCmd, personNotesAppendCmd,
	} {
		command.Flags().BoolVar(&personNotesJSON, flagJSON, false, "Output as JSON")
	}
	for _, command := range []*cobra.Command{personNotesSetCmd, personNotesAppendCmd} {
		command.Flags().StringVar(&personNotesText, "text", "",
			"Notes text, @path, or - for standard input")
	}
	personNotesSetCmd.Flags().Int64Var(&personNotesExpectedID,
		"expected-value-id", 0,
		"Compare-and-swap: the current notes value ID expected to be superseded")
}
