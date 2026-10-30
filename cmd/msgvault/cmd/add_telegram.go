package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/gotd/td/session"
	gotd "github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
	telegramapi "go.kenn.io/msgvault/internal/telegram/api"
)

// registerTelegramAccount holds the lifecycle lock across credential and source setup.
func registerTelegramAccount(ctx context.Context, st *store.Store, dir string, appID int, appHash string, owner *tg.User, data []byte) (*store.Source, error) {
	if owner == nil || owner.ID <= 0 || !owner.Self {
		return nil, errors.New("Telegram account identity is invalid")
	}
	if len(data) == 0 {
		return nil, errors.New("Telegram session is empty")
	}
	manager := telegramapi.NewSessionStore(dir, appID, appHash, owner.ID)
	var source *store.Source
	err := manager.WithLifecycleLockContext(ctx, func() error {
		if err := manager.StoreSession(ctx, data); err != nil {
			return err
		}
		var err error
		source, err = st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(owner.ID))
		if err != nil {
			return fmt.Errorf("register Telegram source: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return source, nil
}

type telegramPrompts struct {
	read func(context.Context, string) (string, error)
}

func newTelegramPrompts(read func(context.Context, string) (string, error)) *telegramPrompts {
	return &telegramPrompts{read: read}
}

func (p *telegramPrompts) Phone(ctx context.Context) (string, error) {
	value, err := p.read(ctx, "Telegram phone number:")
	return strings.TrimSpace(value), err
}
func (p *telegramPrompts) Code(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
	value, err := p.read(ctx, "Telegram verification code:")
	return strings.TrimSpace(value), err
}
func (p *telegramPrompts) Password(ctx context.Context) (string, error) {
	return p.read(ctx, "Telegram two-factor password:")
}
func (*telegramPrompts) AcceptTermsOfService(context.Context, tg.HelpTermsOfService) error {
	return telegramapi.ErrSignupUnsupported
}
func (*telegramPrompts) SignUp(context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, telegramapi.ErrSignupUnsupported
}

// telegramCredentialPayload travels only through the daemon's secret channel.
type telegramCredentialPayload struct {
	AppID   int
	OwnerID int64
	secret  *telegramCredentialSecret
}

type telegramCredentialSecret struct {
	appHash string
	session []byte
}
type telegramCredentialWire struct {
	AppID   int    `json:"api_id"`
	AppHash string `json:"api_hash"`
	OwnerID int64  `json:"owner_id"`
	Session []byte `json:"session"`
}

func newTelegramCredential(appID int, appHash string, ownerID int64, data []byte) telegramCredentialPayload {
	return telegramCredentialPayload{AppID: appID, OwnerID: ownerID, secret: &telegramCredentialSecret{appHash: appHash, session: data}}
}
func (telegramCredentialPayload) String() string     { return "Telegram credential [redacted]" }
func (p telegramCredentialPayload) GoString() string { return p.String() }
func (p telegramCredentialPayload) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, p.String())
}

func encodeTelegramCredential(p telegramCredentialPayload) (string, error) {
	if p.AppID <= 0 || int64(p.AppID) > math.MaxInt32 || p.OwnerID <= 0 || p.secret == nil || p.secret.appHash == "" || len(p.secret.appHash) > 128 || len(p.secret.session) == 0 || len(p.secret.session) > 256*1024 {
		return "", errors.New("invalid Telegram credential")
	}
	data, err := json.Marshal(telegramCredentialWire{AppID: p.AppID, AppHash: p.secret.appHash, OwnerID: p.OwnerID, Session: p.secret.session})
	if err != nil {
		return "", errors.New("cannot encode Telegram credential")
	}
	return string(data), nil
}

func decodeTelegramCredential(encoded string) (telegramCredentialPayload, error) {
	var wire telegramCredentialWire
	if len(encoded) > 512*1024 || json.Unmarshal([]byte(encoded), &wire) != nil {
		return telegramCredentialPayload{}, errors.New("invalid Telegram credential")
	}
	p := newTelegramCredential(wire.AppID, wire.AppHash, wire.OwnerID, wire.Session)
	if _, err := encodeTelegramCredential(p); err != nil {
		return telegramCredentialPayload{}, err
	}
	return p, nil
}

func parseTelegramAppID(value string) (int, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32)
	if err != nil || id <= 0 {
		return 0, errors.New("Telegram api_id must be a positive integer")
	}
	return int(id), nil
}

// newAddTelegramCmd authenticates on the invoking terminal and registers on the daemon.
func newAddTelegramCmd() *cobra.Command {
	return newAddTelegramCmdWithAuthenticate(authenticateTelegram)
}

func newAddTelegramCmdWithAuthenticate(authenticate func(context.Context, int, string, bool, *telegramPrompts, io.Writer) (telegramCredentialPayload, error)) *cobra.Command {
	var useQR bool
	cmd := &cobra.Command{
		Use: "add-telegram", Short: "Register a Telegram account for archival", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if isDaemonCLISubprocess() {
				return registerTelegramFromEnvironment(cmd)
			}
			reader := bufio.NewReader(cmd.InOrStdin())
			read := func(ctx context.Context, title string) (string, error) {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				if file, ok := cmd.InOrStdin().(*os.File); ok && isatty.IsTerminal(file.Fd()) {
					return readPasswordInteractive(title, cmd.ErrOrStderr())
				}
				line, err := reader.ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					return "", telegramapi.ErrLoginInputRead
				}
				value := strings.TrimRight(line, "\r\n")
				if strings.TrimSpace(value) == "" {
					return "", telegramapi.ErrLoginInputRequired
				}
				return value, nil
			}
			rawID, err := read(cmd.Context(), "Telegram api_id:")
			if err != nil {
				return err
			}
			appID, err := parseTelegramAppID(rawID)
			if err != nil {
				return err
			}
			appHash, err := read(cmd.Context(), "Telegram api_hash:")
			if err != nil {
				return err
			}
			credential, err := authenticate(cmd.Context(), appID, strings.TrimSpace(appHash), useQR, newTelegramPrompts(read), cmd.OutOrStdout())
			if err != nil {
				return err
			}
			encoded, err := encodeTelegramCredential(credential)
			if err != nil {
				return err
			}
			return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, args, map[string]string{clirun.EnvTelegramSession: encoded})
		},
	}
	cmd.Flags().BoolVar(&useQR, "qr", false, "Log in by scanning a QR code")
	return cmd
}

func authenticateTelegram(ctx context.Context, appID int, appHash string, qr bool, prompts *telegramPrompts, out io.Writer) (telegramCredentialPayload, error) {
	limiter, maxWait, err := telegramRequestLimits(ctx)
	if err != nil {
		return telegramCredentialPayload{}, err
	}
	memory := &session.StorageMemory{}
	opts := telegramapi.ClientOptionsWithLimits(memory, Version, limiter, maxWait)
	var loggedIn <-chan struct{}
	if qr {
		dispatcher := tg.NewUpdateDispatcher()
		loggedIn = qrlogin.OnLoginToken(dispatcher)
		opts.UpdateHandler = dispatcher
	}
	client := gotd.NewClient(appID, appHash, opts)
	var result telegramCredentialPayload
	var flowErr error
	err = client.Run(ctx, func(ctx context.Context) error {
		if qr {
			_, flowErr = telegramapi.LoginQRWithPassword(ctx, client.API(), appID, appHash, loggedIn, func(_ context.Context, token qrlogin.Token) error { return printTelegramQR(out, token) }, qrlogin.Options{Migrate: client.MigrateTo}, prompts)
		} else {
			flowErr = telegramapi.LoginPhone(ctx, client.API(), appID, appHash, prompts)
		}
		if flowErr != nil {
			return flowErr
		}
		owner, ownerErr := telegramapi.Owner(ctx, client.API())
		if ownerErr != nil {
			flowErr = errors.New("could not verify Telegram account identity")
			return flowErr
		}
		data, sessionErr := memory.LoadSession(ctx)
		if sessionErr != nil {
			flowErr = errors.New("could not load Telegram session")
			return flowErr
		}
		result = newTelegramCredential(appID, appHash, owner.ID, data)
		return nil
	})
	if flowErr != nil {
		return telegramCredentialPayload{}, flowErr
	}
	if err != nil {
		if ctx.Err() != nil {
			return telegramCredentialPayload{}, ctx.Err()
		}
		return telegramCredentialPayload{}, errors.New("could not connect to Telegram")
	}
	return result, nil
}

func registerTelegramFromEnvironment(cmd *cobra.Command) error {
	encoded := os.Getenv(clirun.EnvTelegramSession)
	_ = os.Unsetenv(clirun.EnvTelegramSession)
	credential, err := decodeTelegramCredential(encoded)
	if err != nil {
		return err
	}
	state := invocationFromContext(cmd.Context())
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
	if err != nil {
		return err
	}
	defer cleanup()
	source, err := registerTelegramAccount(cmd.Context(), st, state.cfg.TokensDir(), credential.AppID, credential.secret.appHash, &tg.User{ID: credential.OwnerID, Self: true}, credential.secret.session)
	if err != nil {
		return err
	}
	if err := runPostSourceCreateMigrationsForInvocation(st, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Registered Telegram account: %s\n", source.Identifier)
	return err
}

func init() { rootCmd.AddCommand(newAddTelegramCmd()) }

func printTelegramQR(out io.Writer, token qrlogin.Token) error {
	image, err := token.Image(0)
	if err != nil {
		return errors.New("cannot render Telegram login QR")
	}
	grid, ok := image.(interface{ Black(int, int) bool })
	if !ok {
		return errors.New("unsupported Telegram login QR image")
	}
	// gotd v0.163's QR image uses an eight-pixel scale and a four-cell border.
	size := image.Bounds().Dx()/8 - 8
	if size <= 0 || size > 177 {
		return errors.New("invalid Telegram login QR dimensions")
	}
	var display strings.Builder
	for y := -4; y < size+4; y++ {
		for x := -4; x < size+4; x++ {
			if grid.Black(x, y) {
				display.WriteString("\x1b[40m  ")
			} else {
				display.WriteString("\x1b[47m  ")
			}
		}
		display.WriteString("\x1b[0m\n")
	}
	_, err = io.WriteString(out, display.String())
	return err
}
