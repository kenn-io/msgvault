package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/textutil"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newAddCardDAVCmd() *cobra.Command {
	var schedule, connection string
	var disabled, google bool
	var oauthApp string
	cmd := &cobra.Command{Use: "add-carddav <base-url> <username> | --google <email>", Short: "Discover and configure a CardDAV account", Args: func(cmd *cobra.Command, args []string) error {
		if google {
			return cobra.ExactArgs(1)(cmd, args)
		}
		return cobra.ExactArgs(2)(cmd, args)
	}}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		selector, err := cardDAVCLIConnection(cmd, connection)
		if err != nil {
			return err
		}
		if !google && oauthApp != "" {
			return usageErr(cmd, errors.New("--oauth-app requires --google"))
		}
		var password string
		if !google {
			password, err = readCardDAVPassword()
			if err != nil {
				return err
			}
		}
		baseURL, username := carddav.GoogleDiscoveryURL, args[0]
		if !google {
			baseURL, username = args[0], args[1]
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		body := generated.SaveCardDAVAccountBody{BaseURL: baseURL, Username: username, Password: &password, Enabled: !disabled, Connection: selector}
		if google {
			provider := generated.Google
			body.Provider = &provider
			body.OauthApp = &oauthApp
			body.Password = nil
		}
		if schedule != "" {
			body.Schedule = &schedule
		}
		resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.SaveCardDAVAccountResp, error) {
			return api.SaveCardDAVAccountWithResponse(cmd.Context(), &generated.SaveCardDAVAccountRequestOptions{Body: &body})
		})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Configured CardDAV account %s with %d discovered address books\n",
			textutil.SanitizeTerminal(resp.JSON200.Username), resp.JSON200.Books)
		return nil
	}
	cmd.Flags().StringVar(&connection, "connection", "", "Connection name (default when omitted)")
	cmd.Flags().BoolVar(&google, "google", false, "Use Google Contacts with an authorized OAuth token")
	cmd.Flags().StringVar(&oauthApp, "oauth-app", "", "Named Google OAuth application")
	cmd.Flags().StringVar(&schedule, "schedule", "", "cron schedule for background synchronization")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "save the connection without enabling synchronization")
	return cmd
}

func readCardDAVPassword() (string, error) {
	method, promptOut := choosePasswordStrategy(isatty.IsTerminal(os.Stdin.Fd()), isatty.IsCygwinTerminal(os.Stdin.Fd()), isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd()), isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()))
	var password string
	var err error
	switch method {
	case passwordInteractive:
		password, err = readPasswordInteractive("CardDAV password:", promptOut)
	case passwordPipe:
		password, err = readPasswordFromPipe(os.Stdin)
	default:
		return "", errors.New("cannot read CardDAV password: no terminal or piped stdin available")
	}
	if err != nil {
		return "", err
	}
	if password == "" {
		return "", errors.New("CardDAV password must not be empty")
	}
	return password, nil
}

func newSyncCardDAVCmd() *cobra.Command {
	var full bool
	var connection string
	cmd := &cobra.Command{Use: "sync-carddav", Short: "Synchronize enabled CardDAV connections", Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		selector, err := cardDAVCLIConnection(cmd, connection)
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		body := generated.SyncCardDAVBody{Full: &full, Connection: selector}
		resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.SyncCardDAVResp, error) {
			return api.SyncCardDAVWithResponse(cmd.Context(), &generated.SyncCardDAVRequestOptions{Body: &body})
		})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "CardDAV sync: %d books, %d created, %d updated, %d removed\n", resp.JSON200.Books, resp.JSON200.Created, resp.JSON200.Updated, resp.JSON200.Removed)
		if resp.JSON200.Status != nil && *resp.JSON200.Status != "succeeded" {
			for _, outcome := range resp.JSON200.Connections {
				if outcome.Status != "succeeded" {
					code := "sync_failed"
					if outcome.ErrorCode != nil {
						code = *outcome.ErrorCode
					}
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "CardDAV %s: %s\n", textutil.SanitizeTerminal(outcome.Connection), textutil.SanitizeTerminal(code))
				}
			}
			return fmt.Errorf("CardDAV sync %s", *resp.JSON200.Status)
		}
		return nil
	}
	cmd.Flags().StringVar(&connection, "connection", "", "Sync one saved connection, including disabled connections")
	cmd.Flags().BoolVar(&full, "full", false, "force a full address-book reconciliation")
	return cmd
}

func newCardDAVCmd() *cobra.Command {
	root := &cobra.Command{Use: "carddav", Short: "Manage CardDAV connections, books, and conflicts"}
	books := &cobra.Command{Use: "books", Short: "List discovered CardDAV address books", Args: cobra.NoArgs, RunE: runCardDAVBooks}
	books.Flags().String("connection", "", "List books for one saved connection")
	var writeTarget, subscribed, lookup bool
	setRole := &cobra.Command{Use: "set-role <book-id>", Short: "Set all roles for a CardDAV address book", Args: cobra.ExactArgs(1)}
	setRole.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := cardDAVCLIPositiveID(cmd, args[0])
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		body := generated.UpdateCardDAVBookRolesBody{WriteTarget: writeTarget, Subscribed: subscribed, LookupSource: lookup}
		resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.UpdateCardDAVBookRolesResp, error) {
			return api.UpdateCardDAVBookRolesWithResponse(cmd.Context(), &generated.UpdateCardDAVBookRolesRequestOptions{PathParams: &generated.UpdateCardDAVBookRolesPath{ID: id}, Body: &body})
		})
		if err != nil {
			return err
		}
		return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), resp.JSON200, json.Deterministic(true))
	}
	setRole.Flags().BoolVar(&writeTarget, "write-target", false, "make this the subscribed publication target")
	setRole.Flags().BoolVar(&subscribed, "subscribed", false, "import unbound cards and synchronize changes")
	setRole.Flags().BoolVar(&lookup, "lookup-source", false, "retain cards for identity lookup")
	books.AddCommand(setRole)
	conflicts := &cobra.Command{Use: "conflicts", Short: "Inspect and resolve CardDAV conflicts"}
	conflicts.AddCommand(&cobra.Command{Use: cmdUseList, Short: "List unresolved CardDAV conflicts", Args: cobra.NoArgs, RunE: runCardDAVConflicts})
	conflicts.AddCommand(&cobra.Command{Use: "show <conflict-id>", Short: "Show safe base, local, and remote summaries for a CardDAV conflict", Args: cobra.ExactArgs(1), RunE: runCardDAVConflictShow})
	resolve := &cobra.Command{Use: "resolve <conflict-id> <keep_local|keep_remote>", Short: "Resolve one CardDAV conflict", Args: cobra.ExactArgs(2), RunE: runCardDAVResolve}
	conflicts.AddCommand(resolve)
	root.AddCommand(books, conflicts, newAuthorizeGoogleCardDAVCmd(), &cobra.Command{Use: "connections", Short: "List saved CardDAV connections", Args: cobra.NoArgs, RunE: runCardDAVConnections})
	return root
}

func runCardDAVBooks(cmd *cobra.Command, _ []string) error {
	name, err := cmd.Flags().GetString("connection")
	if err != nil {
		return fmt.Errorf("read CardDAV connection flag: %w", err)
	}
	selector, err := cardDAVCLIConnection(cmd, name)
	if err != nil {
		return err
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ListCardDAVBooksResp, error) {
		return api.ListCardDAVBooksWithResponse(cmd.Context(), &generated.ListCardDAVBooksRequestOptions{Query: &generated.ListCardDAVBooksQuery{Connection: selector}})
	})
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tCONNECTION\tNAME\tWRITE\tSUBSCRIBED\tLOOKUP\tRECONCILE")
	for _, b := range resp.JSON200.Books {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%t\t%t\t%t\t%t\n", b.ID, cardDAVCLIOptionalString(b.Connection),
			textutil.SanitizeTerminal(b.Name), b.WriteTarget, b.Subscribed,
			b.LookupSource, b.NeedsFullReconcile)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush CardDAV address books: %w", err)
	}
	return nil
}
func runCardDAVConflicts(cmd *cobra.Command, _ []string) error {
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ListCardDAVConflictsResp, error) {
		return api.ListCardDAVConflictsWithResponse(cmd.Context())
	})
	if err != nil {
		return err
	}
	return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), resp.JSON200, json.Deterministic(true))
}
func runCardDAVConflictShow(cmd *cobra.Command, args []string) error {
	id, err := cardDAVCLIPositiveID(cmd, args[0])
	if err != nil {
		return err
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.GetCardDAVConflictResp, error) {
		return api.GetCardDAVConflictWithResponse(cmd.Context(), &generated.GetCardDAVConflictRequestOptions{PathParams: &generated.GetCardDAVConflictPath{ID: id}})
	})
	if err != nil {
		return err
	}
	return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), resp.JSON200, json.Deterministic(true))
}
func runCardDAVResolve(cmd *cobra.Command, args []string) error {
	id, err := cardDAVCLIPositiveID(cmd, args[0])
	if err != nil {
		return err
	}
	choice := generated.CardDAVResolveRequestChoice(args[1])
	if err = choice.Validate(); err != nil {
		return usageErr(cmd, err)
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	body := generated.ResolveCardDAVConflictBody{Choice: choice}
	_, err = daemonclient.APIResponseWithStatuses(client, []int{http.StatusOK}, func(api *apiclient.Client) (*generated.ResolveCardDAVConflictResp, error) {
		return api.ResolveCardDAVConflictWithResponse(cmd.Context(), &generated.ResolveCardDAVConflictRequestOptions{PathParams: &generated.ResolveCardDAVConflictPath{ID: id}, Body: &body})
	})
	return err
}
func cardDAVCLIPositiveID(cmd *cobra.Command, raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, usageErr(cmd, errors.New("ID must be a positive integer"))
	}
	return id, nil
}

func newPersonCardDAVCommand(action string, publish bool) *cobra.Command {
	direction := "from"
	if publish {
		direction = "to"
	}
	cmd := &cobra.Command{Use: action + " <person-id>", Short: action + " a person " + direction + " CardDAV", Args: cobra.ExactArgs(1)}
	var preview bool
	var approvalToken string
	if publish {
		cmd.Flags().BoolVar(&preview, "preview", false, "Print the exact vCard and approval token as JSON without publishing")
		cmd.Flags().StringVar(&approvalToken, "approve", "", "Approve a --preview token; conflicts also need explicit keep_local resolution")
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := cardDAVCLIPositiveID(cmd, args[0])
		if err != nil {
			return err
		}
		if preview && approvalToken != "" {
			return usageErr(cmd, errors.New("--preview and --approve cannot be combined"))
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		switch {
		case preview:
			resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.PreviewCardDAVPublicationResp, error) {
				return api.PreviewCardDAVPublicationWithResponse(cmd.Context(), &generated.PreviewCardDAVPublicationRequestOptions{PathParams: &generated.PreviewCardDAVPublicationPath{PersonID: id}})
			})
			if err != nil {
				return err
			}
			return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), resp.JSON200, json.Deterministic(true))
		case approvalToken != "":
			body := generated.ApproveCardDAVPublicationBody{ApprovalToken: approvalToken}
			_, err = daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ApproveCardDAVPublicationResp, error) {
				return api.ApproveCardDAVPublicationWithResponse(cmd.Context(), &generated.ApproveCardDAVPublicationRequestOptions{PathParams: &generated.ApproveCardDAVPublicationPath{PersonID: id}, Body: &body})
			})
		case publish:
			_, err = daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.PublishCardDAVPersonResp, error) {
				return api.PublishCardDAVPersonWithResponse(cmd.Context(), &generated.PublishCardDAVPersonRequestOptions{PathParams: &generated.PublishCardDAVPersonPath{PersonID: id}})
			})
		default:
			_, err = daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.UnpublishCardDAVPersonResp, error) {
				return api.UnpublishCardDAVPersonWithResponse(cmd.Context(), &generated.UnpublishCardDAVPersonRequestOptions{PathParams: &generated.UnpublishCardDAVPersonPath{PersonID: id}})
			})
		}
		if err == nil {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Person %d CardDAV publication updated\n", id)
		}
		return err
	}
	return cmd
}

func init() {
	rootCmd.AddCommand(newAddCardDAVCmd(), newSyncCardDAVCmd(), newCardDAVCmd())
	personCmd.AddCommand(newPersonCardDAVCommand("publish", true), newPersonCardDAVCommand("unpublish", false))
}

func cardDAVCLIConnection(cmd *cobra.Command, name string) (*string, error) {
	if name == "" && !cmd.Flags().Changed("connection") {
		return nil, nil //nolint:nilnil // A nil selector deliberately means aggregate sync or an unfiltered read.
	}
	if err := config.ValidateCardDAVConnectionName(name); err != nil {
		return nil, usageErr(cmd, fmt.Errorf("invalid CardDAV connection: %w", err))
	}
	return &name, nil
}

func cardDAVCLIOptionalString(value *string) string {
	if value == nil {
		return ""
	}
	return textutil.SanitizeTerminal(*value)
}

func runCardDAVConnections(cmd *cobra.Command, _ []string) error {
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ListCardDAVConnectionsResp, error) {
		return api.ListCardDAVConnectionsWithResponse(cmd.Context())
	})
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "CONNECTION\tENABLED\tAVAILABLE\tSTATE")
	for _, connection := range response.JSON200.Connections {
		state := "configured"
		if connection.Orphaned {
			state = "orphaned (restore configuration)"
		}
		_, _ = fmt.Fprintf(w, "%s\t%t\t%t\t%s\n", textutil.SanitizeTerminal(connection.Connection), connection.Status.Enabled, connection.Status.Available, state)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush CardDAV connections: %w", err)
	}
	return nil
}
