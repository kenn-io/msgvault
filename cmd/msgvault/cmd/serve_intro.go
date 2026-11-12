package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/introops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

func introError(err error) error {
	// Never log an underlying error that could contain caller-supplied text.
	return &api.CLIRunCodedError{Code: introops.ErrorCode(err)}
}
func (a *storeAPIAdapter) runCLIIntro(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	if req.Cwd != "" {
		return introError(store.ErrIntroductionInvalid)
	}
	svc := introops.Service{Store: a.store}
	execute := func(cmd *cobra.Command, args []string) error {
		var result any
		var err error
		switch cmd.Name() {
		case "prepare":
			source, _ := cmd.Flags().GetInt64("source-id")
			people, _ := cmd.Flags().GetInt64Slice("person")
			title, _ := cmd.Flags().GetString("title")
			body, _ := cmd.Flags().GetString("body")
			if len(people) != 2 {
				return introError(store.ErrIntroductionInvalid)
			}
			result, err = svc.Prepare(ctx, req.Grant, store.PersonIntroductionPrepare{SourceID: source, PersonAID: people[0], PersonBID: people[1], Title: title, Body: body})
		case "list":
			source, _ := cmd.Flags().GetInt64("source-id")
			limit, _ := cmd.Flags().GetInt("limit")
			if (cmd.Flags().Changed("limit") && limit == 0) || (cmd.Flags().Changed("source-id") && source <= 0) {
				return introError(store.ErrIntroductionInvalid)
			}
			after, _ := cmd.Flags().GetString("after-id")
			result, err = svc.List(ctx, req.Grant, store.PersonIntroductionQuery{SourceID: source, Limit: limit, AfterID: after})
		case "show":
			result, err = svc.Get(ctx, req.Grant, args[0])
		case "edit":
			revision, _ := cmd.Flags().GetInt64("revision")
			in := store.PersonIntroductionEdit{Revision: revision}
			if cmd.Flags().Changed("title") {
				v, _ := cmd.Flags().GetString("title")
				in.Title = &v
			}
			if cmd.Flags().Changed("body") {
				v, _ := cmd.Flags().GetString("body")
				in.Body = &v
			}
			result, err = svc.Edit(ctx, req.Grant, args[0], in)
		case "cancel":
			revision, _ := cmd.Flags().GetInt64("revision")
			result, err = svc.Cancel(ctx, req.Grant, args[0], revision)
		default:
			return introError(store.ErrIntroductionInvalid)
		}
		if err != nil {
			return introError(err)
		}
		asJSON, _ := cmd.Flags().GetBool("json")
		var output string
		if asJSON {
			data, err := json.Marshal(result)
			if err != nil {
				return introError(err)
			}
			output = string(data) + "\n"
		} else {
			var b strings.Builder
			render := func(row introops.Record, detail bool) {
				fmt.Fprintf(&b, "%s revision=%d status=%s source=%d account=%s people=%d,%d title=%s\n", textutil.SanitizeTerminal(row.IntroID), row.Revision, textutil.SanitizeTerminal(row.Status), row.SourceID, textutil.SanitizeTerminal(row.SourceIdentifier), row.PersonAID, row.PersonBID, textutil.SanitizeTerminal(row.Title))
				for _, blocker := range row.Blockers {
					fmt.Fprintf(&b, "  blocker=%s person=%d\n", textutil.SanitizeTerminal(blocker.Code), blocker.PersonID)
				}
				if detail && row.Body != "" {
					fmt.Fprintf(&b, "body:\n%s\n", textutil.SanitizeTerminalMultiline(row.Body))
				}
			}
			switch value := result.(type) {
			case introops.Record:
				render(value, true)
			case introops.Page:
				for _, row := range value.Items {
					render(row, false)
				}
				if len(value.Items) == 0 {
					b.WriteString("No introductions.\n")
				}
				if value.HasMore {
					fmt.Fprintf(&b, "next: %s\n", value.NextAfterID)
				}
			}
			output = b.String()
		}
		if emit != nil {
			if err := emit(api.CLIRunEvent{Type: "stdout", Data: output}); err != nil {
				return introError(err)
			}
		}
		return nil
	}
	root := newIntroCommand(execute)
	clirun.AddLoggingFlags(root.PersistentFlags())
	root.SilenceErrors = true
	root.SilenceUsage = true
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs(req.Args[1:])
	if err := root.ExecuteContext(ctx); err != nil {
		if coded, ok := errors.AsType[*api.CLIRunCodedError](err); ok {
			return coded
		}
		return introError(store.ErrIntroductionInvalid)
	}
	return nil
}
