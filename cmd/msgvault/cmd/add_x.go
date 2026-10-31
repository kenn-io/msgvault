package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/oauth"
	xapi "go.kenn.io/msgvault/internal/x"
)

type xRegistrationOutput struct {
	SourceID   int64  `json:"source_id"`
	UserID     string `json:"user_id"`
	Username   string `json:"username"`
	Generation int64  `json:"generation"`
}

func emitXRegistration(result xRegistrationOutput, asJSON bool, emit func(api.CLIRunEvent) error) error {
	var data string
	if asJSON {
		body, err := json.Marshal(result)
		if err != nil {
			return err
		}
		data = string(body) + "\n"
	} else {
		data = fmt.Sprintf("Registered X account @%s (%s), source %d.\n", result.Username, result.UserID, result.SourceID)
	}
	if emit == nil {
		return nil
	}
	return emit(api.CLIRunEvent{Type: "stdout", Data: data})
}

func newAddXCmd(deps xCommandDeps) *cobra.Command {
	var clientID, clientType, userID string
	var callbackPort int
	var allowSend, headless, register, asJSON bool
	command := &cobra.Command{
		Use: "add-x", Short: "Authorize X locally or register on the daemon host",
		Long: `Authorize an X account with a local browser and stage its protected credentials.
Use --allow-send to request tweet.write in addition to archival scopes.
For a remote daemon, securely copy the staged file to its host before using
--register --user-id. Registration verifies the credential on that host.`, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			inv := invocationFromContext(cmd.Context())
			if inv == nil || inv.cfg == nil {
				return errors.New("X authorization requires configuration")
			}
			if isAgentMode(inv) {
				return errors.New("X account setup requires owner authority")
			}
			if register {
				if err := xapi.ValidateID(userID); err != nil {
					return usageErr(cmd, err)
				}
				for _, name := range []string{"client-id", "client-type", "callback-port", "allow-send", "headless"} {
					if cmd.Flags().Changed(name) {
						return usageErr(cmd, errors.New("--register accepts only --user-id and optional --json"))
					}
				}
				return runXRegistrationHTTP(cmd, userID, asJSON)
			}
			if cmd.Flags().Changed("user-id") || cmd.Flags().Changed("json") {
				return usageErr(cmd, errors.New("--user-id and --json require --register"))
			}
			if clientType != string(xapi.PublicClient) && clientType != string(xapi.ConfidentialClient) {
				return usageErr(cmd, errors.New("--client-type must be public or confidential"))
			}
			if headless {
				fmt.Fprintln(cmd.OutOrStdout(), "On a machine with a browser, authorize locally:")
				args := []string{"msgvault", "add-x", "--client-id", clientID}
				if clientID == "" {
					args[len(args)-1] = "YOUR_CLIENT_ID"
				}
				if allowSend {
					args = append(args, "--allow-send")
				}
				if clientType == string(xapi.ConfidentialClient) {
					args = append(args, "--client-type", clientType)
				}
				if cmd.Flags().Changed("callback-port") {
					args = append(args, "--callback-port", strconv.Itoa(callbackPort))
				}
				oauth.PrintHeadlessCommand(cmd.OutOrStdout(), args...)
				fmt.Fprintln(cmd.OutOrStdout(), "Securely copy the staged credential to the daemon host's token directory under .x-staged/, retaining mode 0600. Then register it:")
				oauth.PrintHeadlessCommand(cmd.OutOrStdout(), "msgvault", "add-x", "--register", "--user-id", "NUMERIC_USER_ID")
				return nil
			}
			if strings.TrimSpace(clientID) == "" {
				return usageErr(cmd, errors.New("--client-id is required for local authorization"))
			}
			opts := deps.oauthOptions
			opts.ClientID = clientID
			opts.ClientType = xapi.ClientType(clientType)
			opts.AllowSend = allowSend
			opts.Output = cmd.ErrOrStderr()
			if cmd.Flags().Changed("callback-port") {
				opts.CallbackPort = callbackPort
			}
			if deps.httpClient != nil {
				opts.HTTPClient = deps.httpClient
			}
			if deps.apiBaseURL != "" {
				opts.APIBaseURL = deps.apiBaseURL
			}
			if opts.ClientType == xapi.ConfidentialClient && opts.ReadSecret == nil {
				opts.ReadSecret = func(context.Context) (string, error) { return readXClientSecret(cmd.InOrStdin()) }
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "Authorization verifies your numeric owner with one X profile read. Host registration verifies its staged credential with another profile read.")
			record, err := xapi.Authorize(cmd.Context(), opts)
			if err != nil {
				return err
			}
			manager := deps.manager(xStagedTokensDir(inv.cfg))
			if err := manager.WithLifecycleLock(cmd.Context(), record.OwnerID, func() error { return manager.Save(cmd.Context(), record) }); err != nil {
				return err
			}
			if isRemoteModeFor(inv) {
				fmt.Fprintf(cmd.OutOrStdout(), "Staged protected X credentials at %s/x_%s.json.\n", xStagedTokensDir(inv.cfg), record.OwnerID)
				fmt.Fprintln(cmd.OutOrStdout(), "Securely copy this file into the daemon host's token directory under .x-staged/, retaining mode 0600. After registration, the daemon owns refresh; do not run another installation with the copied token. Then run against that daemon:")
				oauth.PrintHeadlessCommand(cmd.OutOrStdout(), "msgvault", "add-x", "--register", "--user-id", record.OwnerID)
				return nil
			}
			return runXRegistrationHTTP(cmd, record.OwnerID, false)
		},
	}
	command.Flags().StringVar(&clientID, "client-id", "", "X OAuth application client ID")
	command.Flags().StringVar(&clientType, "client-type", "public", "OAuth client type: public or confidential")
	command.Flags().IntVar(&callbackPort, "callback-port", xapi.DefaultCallbackPort, "IPv4 loopback OAuth callback port")
	command.Flags().BoolVar(&allowSend, "allow-send", false, "Request tweet.write permission")
	command.Flags().BoolVar(&headless, "headless", false, "Print browser-machine authorization and host registration instructions")
	command.Flags().BoolVar(&register, "register", false, "Verify host-local credentials and register the selected daemon's source")
	command.Flags().StringVar(&userID, "user-id", "", "Canonical numeric owner ID for host registration")
	command.Flags().BoolVar(&asJSON, "json", false, "Print registration details as JSON")
	return command
}

// X setup intentionally sends no workstation environment or working directory.
func runXRegistrationHTTP(cmd *cobra.Command, userID string, asJSON bool) error {
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	args := []string{"add-x", "--register", "--user-id", userID}
	if asJSON {
		args = append(args, "--json")
	}
	return client.RunCLICommand(cmd.Context(), daemonclient.CLIRunRequest{Args: args}, func(stream, data string) error {
		switch stream {
		case cliStreamStdout:
			_, err := fmt.Fprint(cmd.OutOrStdout(), data)
			return err
		case cliStreamStderr:
			_, err := fmt.Fprint(cmd.ErrOrStderr(), data)
			return err
		}
		return nil
	})
}

func readXClientSecret(in io.Reader) (string, error) {
	if secret := os.Getenv("MSGVAULT_X_CLIENT_SECRET"); secret != "" {
		return secret, nil
	}
	if in != os.Stdin {
		return readPasswordFromPipe(in)
	}
	method, output := choosePasswordStrategy(isatty.IsTerminal(os.Stdin.Fd()), isatty.IsCygwinTerminal(os.Stdin.Fd()), isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd()), isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()))
	switch method {
	case passwordInteractive:
		return readPasswordInteractive("X client secret:", output)
	case passwordPipe:
		return readPasswordFromPipe(in)
	default:
		return "", errors.New("cannot read X client secret: pipe it through stdin or set MSGVAULT_X_CLIENT_SECRET")
	}
}

func init() { rootCmd.AddCommand(newAddXCmd(defaultXCommandDeps())) }
