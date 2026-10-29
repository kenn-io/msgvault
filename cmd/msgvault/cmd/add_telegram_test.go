package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/telegram/api"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRegisterTelegramAccountUsesExportSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	dir := filepath.Join(t.TempDir(), "tokens")
	owner := &tg.User{ID: 456, Self: true}
	source, err := registerTelegramAccount(context.Background(), st, dir, 123, "synthetic-hash", owner, []byte("synthetic-session"))
	require.NoError(err)
	assert.Equal("telegram", source.SourceType)
	assert.Equal("user456", source.Identifier)
	session, err := api.OpenSessionStore(dir, 456)
	require.NoError(err)
	data, err := session.LoadSession(context.Background())
	require.NoError(err)
	assert.Equal([]byte("synthetic-session"), data)
	info, err := os.Stat(filepath.Join(dir, "telegram_456.json"))
	require.NoError(err)
	assert.True(info.Mode().IsRegular())
	again, err := registerTelegramAccount(context.Background(), st, dir, 123, "synthetic-hash", owner, []byte("new-session"))
	require.NoError(err)
	assert.Equal(source.ID, again.ID)
}

func TestTelegramPromptsRequestSecretsOnlyWhenNeeded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var prompts []string
	read := func(_ context.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return " synthetic ", nil
	}
	auth := newTelegramPrompts(read)
	phone, err := auth.Phone(context.Background())
	require.NoError(err)
	assert.Equal("synthetic", phone)
	code, err := auth.Code(context.Background(), &tg.AuthSentCode{})
	require.NoError(err)
	assert.Equal("synthetic", code)
	password, err := auth.Password(context.Background())
	require.NoError(err)
	assert.Equal(" synthetic ", password)
	assert.Len(prompts, 3)
	assert.Error(auth.AcceptTermsOfService(context.Background(), tg.HelpTermsOfService{}))
	_, err = auth.SignUp(context.Background())
	assert.Error(err)
}

func TestTelegramCredentialPayloadRoundTripAndRedaction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	value := newTelegramCredential(123, "synthetic-hash", 456, []byte("synthetic-session"))
	encoded, err := encodeTelegramCredential(value)
	require.NoError(err)
	decoded, err := decodeTelegramCredential(encoded)
	require.NoError(err)
	assert.Equal(value, decoded)
	for _, format := range []string{"%v", "%+v", "%#v", "%p"} {
		printed := fmt.Sprintf(format, value)
		assert.NotContains(printed, "synthetic-hash")
		assert.NotContains(printed, "synthetic-session")
	}
	_, err = decodeTelegramCredential("not-json")
	require.Error(err)
	assert.NotContains(err.Error(), "not-json")
}

func TestParseTelegramAppID(t *testing.T) {
	id, err := parseTelegramAppID(" 123 ")
	require.NoError(t, err)
	assert.Equal(t, 123, id)
	for _, input := range []string{"", "0", "-1", "2147483648", "not-a-number"} {
		_, err := parseTelegramAppID(input)
		assert.Error(t, err)
	}
}

func TestPrintTelegramQRDoesNotPrintToken(t *testing.T) {
	assert := assert.New(t)

	var output bytes.Buffer
	token := qrlogin.NewToken([]byte("synthetic-secret-token"), int(time.Now().Add(time.Minute).Unix()))
	require.NoError(t, printTelegramQR(&output, token))
	assert.Contains(output.String(), "\x1b[40m")
	assert.Contains(output.String(), "\x1b[47m")
	assert.NotContains(output.String(), "synthetic-secret-token")
	assert.NotContains(output.String(), token.URL())
}

func TestAddTelegramRoutesAuthenticatedSessionToDaemon(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal(t, []string{"add-telegram"}, req.Args)
		p, err := decodeTelegramCredential(req.Env[clirun.EnvTelegramSession])
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, int64(456), p.OwnerID)
		assert.Equal(t, "synthetic-hash", p.secret.appHash)
		assert.Equal(t, []byte("synthetic-session"), p.secret.session)
	}, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(daemonCLISubprocessEnv, "")
	login := func(ctx context.Context, appID int, appHash string, qr bool, prompts *telegramPrompts, _ io.Writer) (telegramCredentialPayload, error) {
		assert.False(t, qr)
		mock := tgmock.NewRequire(t)
		mock.Expect().ThenResult(&tg.AuthSentCode{Type: &tg.AuthSentCodeTypeApp{Length: 5}, PhoneCodeHash: "code-hash"})
		mock.Expect().ThenResult(&tg.AuthAuthorization{User: &tg.User{ID: 456}})
		require.NoError(t, api.LoginPhone(ctx, tg.NewClient(mock), appID, appHash, prompts))
		assert.True(t, mock.AllWereMet())
		return newTelegramCredential(appID, appHash, 456, []byte("synthetic-session")), nil
	}
	cmd := newAddTelegramCmdWithAuthenticate(login)
	cmd.SetContext(ctx)
	cmd.SetIn(strings.NewReader("123\n synthetic-hash \n+15550100100\n12345\n"))
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	require.NoError(t, cmd.Execute())
	{
		assert := assert.New(t)
		assert.EqualValues(1, requests.Load())
	}
	assert.NotContains(t, output.String(), "synthetic-hash")
	assert.NotContains(t, output.String(), "synthetic-session")
}

func TestAddTelegramReportsEmptyPhoneInput(t *testing.T) {
	login := func(ctx context.Context, appID int, appHash string, _ bool, prompts *telegramPrompts, _ io.Writer) (telegramCredentialPayload, error) {
		mock := tgmock.NewRequire(t)
		return telegramCredentialPayload{}, api.LoginPhone(ctx, tg.NewClient(mock), appID, appHash, prompts)
	}
	cmd := newAddTelegramCmdWithAuthenticate(login)
	cmd.SetIn(strings.NewReader("123\nsynthetic-hash\n\n"))
	err := cmd.Execute()
	require.ErrorContains(t, err, "Telegram login input is required")
}
