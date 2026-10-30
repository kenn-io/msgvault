package cmd

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/threads"
)

type threadsOAuthPayload struct {
	AppID       string `json:"app_id"`
	RedirectURI string `json:"redirect_uri"`
	Code        string `json:"code"`
	AppSecret   string `json:"app_secret"`
}

func (threadsOAuthPayload) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "Threads OAuth handoff [redacted]")
}

func newAddThreadsCmd(deps threadsCommandDeps) *cobra.Command {
	cmd := newAddThreadsLocalCmd(deps)
	local := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if isAgentMode(invocationFromCommand(cmd)) {
			return errors.New("Threads connection requires the owner")
		}
		if isDaemonCLISubprocess() {
			return local(cmd, args)
		}
		appID, _ := cmd.Flags().GetString("app-id")
		redirect, _ := cmd.Flags().GetString("redirect-uri")
		allowSend, _ := cmd.Flags().GetBool("allow-send")
		state, err := threads.NewOAuthState()
		if err != nil {
			return err
		}
		authorization, err := threads.AuthorizationURL(appID, redirect, state, allowSend)
		if err != nil {
			return usageErr(cmd, err)
		}
		reader := bufio.NewReader(cmd.InOrStdin())
		secret := os.Getenv("MSGVAULT_THREADS_APP_SECRET")
		if secret == "" {
			file, terminal := cmd.InOrStdin().(*os.File)
			if !terminal || !isatty.IsTerminal(file.Fd()) {
				return errors.New("set MSGVAULT_THREADS_APP_SECRET or use a terminal for the masked prompt")
			}
			secret, err = readPasswordInteractive("Threads app secret:", cmd.ErrOrStderr())
			if err != nil {
				return errors.New("read Threads app secret")
			}
			secret = strings.TrimSpace(secret)
		}
		if secret == "" {
			return errors.New("Threads app secret is required")
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Open this authorization URL in your browser:\n%s\n", authorization)
		_, _ = fmt.Fprint(cmd.ErrOrStderr(), "Paste the full returned callback URL: ")
		callback, err := reader.ReadString('\n')
		if err != nil && !(errors.Is(err, io.EOF) && callback != "") {
			return errors.New("read Threads callback")
		}
		code, err := threads.ParseOAuthCallback(redirect, strings.TrimSpace(callback), state)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(threadsOAuthPayload{AppID: appID, RedirectURI: redirect, Code: code, AppSecret: secret})
		if err != nil {
			return errors.New("encode Threads authorization handoff")
		}
		return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, args, map[string]string{clirun.EnvThreadsOAuth: string(payload)})
	}
	return cmd
}

func newAddThreadsLocalCmd(deps threadsCommandDeps) *cobra.Command {
	var appID, redirectURI string
	var allowSend bool
	cmd := &cobra.Command{
		Use: "add-threads", Short: "Connect your Threads account for archiving posts and replies", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if isAgentMode(invocationFromCommand(cmd)) {
				return errors.New("Threads connection requires the owner")
			}
			if invocationFromCommand(cmd) != nil && deps.bind != nil {
				deps = deps.bind(cmd.Context())
			}
			return runAddThreads(cmd, deps, appID, redirectURI, allowSend)
		},
	}
	cmd.Flags().StringVar(&appID, "app-id", "", "Threads app ID")
	cmd.Flags().StringVar(&redirectURI, "redirect-uri", "", "Registered HTTPS callback")
	cmd.Flags().BoolVar(&allowSend, "allow-send", false, "Request publishing scopes (does not enable sending)")
	return cmd
}

func runAddThreads(cmd *cobra.Command, deps threadsCommandDeps, appID, redirectURI string, allowSend bool) error {
	if _, err := threads.AuthorizationURL(appID, redirectURI, "validate-registration", allowSend); err != nil {
		return usageErr(cmd, err)
	}
	raw := os.Getenv(clirun.EnvThreadsOAuth)
	if deps.oauthPayload != nil {
		raw = deps.oauthPayload()
	}
	if len(raw) > 64<<10 {
		return errors.New("Threads OAuth handoff is too large")
	}
	var payload threadsOAuthPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil || payload.AppID != appID || payload.RedirectURI != redirectURI || payload.Code == "" || payload.AppSecret == "" {
		return errors.New("invalid Threads OAuth handoff; run add-threads from the CLI")
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	sources, err := st.ListSourcesContext(cmd.Context(), sourceTypeThreads)
	if err != nil {
		return err
	}
	initial := make(map[string]int64, len(sources))
	for _, source := range sources {
		initial[source.Identifier] = source.ID
	}
	now := time.Now
	if deps.now != nil {
		now = deps.now
	}
	budget := threads.NewRequestBudget(threads.BudgetOptions{MaxAttempts: threads.ManualRequestLimit, Now: now, Wait: deps.wait})
	scopes := []string{"threads_basic", "threads_read_replies"}
	if allowSend {
		scopes = append(scopes, "threads_content_publish", "threads_manage_replies")
	}
	ctx, cancel := budget.RunContext(cmd.Context())
	defer cancel()
	manager := deps.tokenManager()
	options := deps.oauthOptions(budget)
	options.WithOwnerLock = func(ctx context.Context, identifier string, exchange func() (threads.TokenRecord, error)) (record threads.TokenRecord, retErr error) {
		retErr = manager.WithSourceLock(ctx, identifier, func() (retErr error) {
			var execution *store.SyncExecution
			defer func() {
				if execution != nil {
					retErr = errors.Join(retErr, execution.Release())
				}
			}()
			latest, err := st.ListSourcesContext(ctx, sourceTypeThreads)
			if err != nil {
				return err
			}
			var current *store.Source
			for _, candidate := range latest {
				if candidate.Identifier == identifier {
					current = candidate
					break
				}
			}
			if oldID, existed := initial[identifier]; existed && (current == nil || current.ID != oldID) {
				return errors.New("Threads source changed during authorization; reconnect explicitly")
			}
			if current != nil {
				if current.HistoryOnly || current.MergedIntoSourceID != 0 {
					return errors.New("Threads source is no longer writable")
				}
				execution, err = st.AcquireSyncExecutionContext(ctx, current.ID)
				if err != nil {
					return err
				}
			}
			if deps.limiter != nil {
				budget.BindLimiter(deps.limiter(identifier))
			}
			record, err = exchange()
			if err != nil {
				return err
			}
			verified, err := threads.SourceIdentifier(record.AppID, record.OwnerID)
			if err != nil || verified != identifier {
				return threads.ErrReauthorizationRequired
			}
			if err := manager.Save(record); err != nil {
				return err
			}
			saved, err := manager.Load(record.AppID, record.OwnerID)
			if err != nil {
				return err
			}
			source, err := deps.registerSource(ctx, st, saved, now())
			if err != nil {
				return fmt.Errorf("Threads credential was saved, but account registration failed: %w", err)
			}
			if execution == nil {
				execution, err = st.AcquireSyncExecutionContext(ctx, source.ID)
				if err != nil {
					return err
				}
			}
			if deps.postSourceMigrations != nil {
				if err := deps.postSourceMigrations(st); err != nil {
					return fmt.Errorf("Threads credential was saved, but registration migrations failed: %w", err)
				}
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Connected Threads account: %s\nUse this exact identifier in [[accounts]] to enable scheduled reads.\n", identifier)
			return nil
		})
		return record, retErr
	}
	_, err = threads.ExchangeCredential(ctx, options, threads.NewOAuthRequest(appID, redirectURI, payload.Code, payload.AppSecret, scopes))
	return err
}

func registerThreadsSource(ctx context.Context, st *store.Store, record threads.TokenRecord, ceiling time.Time) (*store.Source, error) {
	identifier, err := threads.SourceIdentifier(record.AppID, record.OwnerID)
	if err != nil {
		return nil, err
	}
	source, err := st.GetOrCreateSourceContext(ctx, sourceTypeThreads, identifier)
	if err != nil {
		return nil, err
	}
	if source.HistoryOnly || source.MergedIntoSourceID != 0 {
		return nil, errors.New("Threads source is retired")
	}
	label := record.Name
	if label == "" {
		label = record.Username
	}
	if label == "" {
		label = "Threads account " + record.OwnerID
	}
	if err := updateSourceDisplayNameForRegistration(st, source.ID, label, nil); err != nil {
		return nil, err
	}
	if _, err := st.EnsureParticipantByIdentifierContext(ctx, "threads_user_id", identifier, label); err != nil {
		return nil, err
	}
	if err := st.AddAccountIdentityContext(ctx, source.ID, identifier, "threads_user_id"); err != nil {
		return nil, err
	}
	if !source.SyncConfig.Valid || source.SyncConfig.String == "" {
		if !source.CreatedAt.IsZero() {
			ceiling = source.CreatedAt
		}
		configuration, err := json.Marshal(map[string]any{"version": 1, "history_floor": time.Unix(1688540400, 0).UTC(), "history_ceiling": ceiling.UTC(), "coverage_generation": 1})
		if err != nil {
			return nil, err
		}
		if err := st.UpdateSourceSyncConfig(source.ID, string(configuration)); err != nil {
			return nil, err
		}
	}
	return st.GetSourceByIDContext(ctx, source.ID)
}

func init() { rootCmd.AddCommand(newAddThreadsCmd(defaultThreadsCommandDeps())) }
