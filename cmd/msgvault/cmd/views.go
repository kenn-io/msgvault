package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/savedview"
	"go.kenn.io/msgvault/internal/store"
)

const viewsMinAPISchemaVersion = "3.45.0"

type viewsClient interface {
	savedview.Service
	APISchemaVersion(context.Context) (string, error)
}
type viewsOpen func(context.Context) (viewsClient, func(), error)

func openViews(ctx context.Context) (viewsClient, func(), error) {
	client, _, err := OpenHTTPStore(ctx)
	if err != nil {
		return nil, func() {}, err
	}
	return client, func() { _ = client.Close() }, nil
}
func resolveCLIView(ctx context.Context, client viewsClient, value string) (*store.SavedView, error) {
	if id, err := strconv.ParseInt(value, 10, 64); err == nil && id > 0 {
		return client.GetSavedView(ctx, id)
	}
	views, err := client.ListSavedViews(ctx)
	if err != nil {
		return nil, err
	}
	for _, view := range views {
		if view.Name == value {
			return client.GetSavedView(ctx, view.ID)
		}
	}
	return nil, store.ErrSavedViewNotFound
}
func newViewsCommand(open viewsOpen) *cobra.Command {
	root := &cobra.Command{Use: "views", Short: "Run Saved Views and pin or hide individual entries"}
	for _, action := range []string{"show", "run", "pin", "unpin", "exclude", "unexclude"} {
		var jsonOutput bool
		var limit int
		var cursor string
		command := &cobra.Command{Use: action + " <view>", Short: map[string]string{"show": "Show a Saved View and its items", "run": "Run a Saved View", "pin": "Pin entries into a Saved View", "unpin": "Remove pins from a Saved View", "exclude": "Hide entries in a Saved View", "unexclude": "Remove hidden entries from a Saved View"}[action]}
		if action == "show" || action == "run" {
			command.Args = cobra.ExactArgs(1)
		} else {
			command.Use += " <entry-key>..."
			command.Args = cobra.MinimumNArgs(2)
		}
		command.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
		if action == "run" {
			command.Flags().IntVar(&limit, "limit", 100, "Page size (1–100)")
			command.Flags().StringVar(&cursor, "cursor", "", "Cursor from the previous page")
		}
		command.RunE = func(cmd *cobra.Command, args []string) error {
			client, cleanup, err := open(cmd.Context())
			if err != nil {
				return err
			}
			defer cleanup()
			version, err := client.APISchemaVersion(cmd.Context())
			if err != nil {
				return err
			}
			if !daemonclient.APISchemaVersionAtLeast(version, viewsMinAPISchemaVersion) {
				return fmt.Errorf("Saved View items require daemon API schema %s or newer; upgrade the daemon", viewsMinAPISchemaVersion)
			}
			view, err := resolveCLIView(cmd.Context(), client, args[0])
			if err != nil {
				return err
			}
			printJSON := func(value any) error {
				data, err := json.Marshal(value)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
				return err
			}
			switch action {
			case "show":
				if jsonOutput {
					return printJSON(view)
				}
				if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s (revision %d)\n%s\n", view.Name, view.Revision, view.CanonicalState); err != nil {
					return err
				}
				if view.Items != nil {
					for _, mode := range []struct {
						prefix string
						items  []store.SavedViewItem
					}{{"+", view.Items.Include}, {"-", view.Items.Exclude}} {
						for _, item := range mode.items {
							if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", mode.prefix, item.EntryKey); err != nil {
								return err
							}
						}
					}
				}
				return nil
			case "run":
				page, err := client.RunSavedView(cmd.Context(), view.ID, limit, cursor)
				if err != nil {
					return err
				}
				if jsonOutput {
					return printJSON(page)
				}
				pinned := map[string]bool{}
				if page.View.Items != nil {
					for _, item := range page.View.Items.Include {
						pinned[item.EntryKey] = true
					}
				}
				for _, row := range page.Rows {
					prefix := " "
					if pinned[row.Key] {
						prefix = "+"
					}
					if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s  %s  %s  %s\n", prefix, row.Kind, row.OccurredAt.Format(time.RFC3339), row.Title, row.Key); err != nil {
						return err
					}
				}
				for _, group := range page.Groups {
					if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s  %d\n", group.Key, group.Count); err != nil {
						return err
					}
				}
				for _, file := range page.Files {
					if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s  %s\n", file.Filename, file.EntryKey); err != nil {
						return err
					}
				}
				if page.NextCursor != "" {
					_, err = fmt.Fprintln(cmd.OutOrStdout(), "next_cursor:", page.NextCursor)
				}
				return err
			default:
				patch := savedview.Patch{}
				switch action {
				case "pin":
					patch.IncludeAdd = args[1:]
				case "unpin":
					patch.IncludeRemove = args[1:]
				case "exclude":
					patch.ExcludeAdd = args[1:]
				case "unexclude":
					patch.ExcludeRemove = args[1:]
				}
				var updated *store.SavedView
				for attempt := 0; attempt < 2; attempt++ {
					updated, err = client.UpdateSavedView(cmd.Context(), view.ID, view.Revision, patch)
					if !errors.Is(err, store.ErrSavedViewRevisionConflict) || attempt == 1 {
						break
					}
					view, err = client.GetSavedView(cmd.Context(), view.ID)
					if err != nil {
						return err
					}
				}
				if err != nil {
					return err
				}
				if jsonOutput {
					return printJSON(updated)
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: %d entries applied\n", action, len(args)-1)
				return err
			}
		}
		root.AddCommand(command)
	}
	return root
}
func init() { rootCmd.AddCommand(newViewsCommand(openViews)) }
