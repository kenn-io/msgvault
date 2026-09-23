package peoplesweep

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	ErrCodexAuthAccountChanged = errors.New("codex account identity changed")
	ErrCodexAuthSourceChanged  = errors.New("codex dedicated auth changed during inference")
	ErrCodexAuthRefreshUnsafe  = errors.New("codex refreshed auth is unsafe")
)

// A single daemon can hold one auth context across login, model discovery, and
// inference. The source-file snapshot below also detects uncoordinated writes.
var codexAuthOperationLocks sync.Map

func lockCodexAuthOperation(ctx context.Context, authHome string) (func(), error) {
	if authHome == "" {
		return func() {}, nil
	}
	created := make(chan struct{}, 1)
	created <- struct{}{}
	value, _ := codexAuthOperationLocks.LoadOrStore(authHome, created)
	gate, ok := value.(chan struct{})
	if !ok {
		return nil, ErrCodexAuthRefreshUnsafe
	}
	select {
	case <-gate:
		return func() { gate <- struct{}{} }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type codexAccountIdentity struct {
	userID      string
	workspaceID string
}

func codexAccountIdentityFromAuth(contents []byte) (codexAccountIdentity, error) {
	var auth struct {
		Mode   string  `json:"auth_mode"`
		APIKey *string `json:"OPENAI_API_KEY"`
		Tokens *struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if decodeSingleJSON(contents, &auth) != nil || auth.Mode != "chatgpt" || auth.APIKey != nil || auth.Tokens == nil ||
		auth.Tokens.AccessToken == "" || auth.Tokens.RefreshToken == "" || !safeProviderMetadata(auth.Tokens.AccountID) {
		return codexAccountIdentity{}, ErrCodexAuthRefreshUnsafe
	}
	parts := strings.Split(auth.Tokens.IDToken, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return codexAccountIdentity{}, ErrCodexAuthRefreshUnsafe
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) > 16<<10 {
		return codexAccountIdentity{}, ErrCodexAuthRefreshUnsafe
	}
	var claims struct {
		Auth struct {
			UserID      string `json:"chatgpt_user_id"`
			WorkspaceID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if decodeSingleJSON(payload, &claims) != nil || !safeProviderMetadata(claims.Auth.UserID) ||
		!safeProviderMetadata(claims.Auth.WorkspaceID) || claims.Auth.WorkspaceID != auth.Tokens.AccountID {
		return codexAccountIdentity{}, ErrCodexAuthRefreshUnsafe
	}
	return codexAccountIdentity{userID: claims.Auth.UserID, workspaceID: claims.Auth.WorkspaceID}, nil
}

func readPrivateCodexAuth(directory string) ([]byte, os.FileInfo, error) {
	dirInfo, err := os.Lstat(directory) //nolint:gosec // The daemon-selected auth directory is checked for private mode and ownership below.
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode().Perm()&0o077 != 0 || !codexAuthOwnedByDaemon(dirInfo) {
		return nil, nil, ErrCodexAuthRefreshUnsafe
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, nil, ErrCodexAuthRefreshUnsafe
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat("auth.json")
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() <= 0 ||
		info.Size() > 1<<20 || !codexAuthOwnedByDaemon(info) {
		return nil, nil, ErrCodexAuthRefreshUnsafe
	}
	file, err := root.Open("auth.json")
	if err != nil {
		return nil, nil, ErrCodexAuthRefreshUnsafe
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !codexAuthOwnedByDaemon(opened) {
		return nil, nil, ErrCodexAuthRefreshUnsafe
	}
	contents, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || len(contents) == 0 || len(contents) > 1<<20 {
		return nil, nil, ErrCodexAuthRefreshUnsafe
	}
	finalInfo, err := file.Stat()
	if err != nil || !os.SameFile(opened, finalInfo) || finalInfo.Size() != int64(len(contents)) ||
		!finalInfo.ModTime().Equal(opened.ModTime()) {
		return nil, nil, ErrCodexAuthRefreshUnsafe
	}
	return contents, opened, nil
}

type codexRefreshState struct {
	authHome     string
	workRoot     string
	sourceInfo   os.FileInfo
	sourceDigest [sha256.Size]byte
	identity     codexAccountIdentity
	identityOK   bool
}

func prepareCodexRefresh(authHome, workRoot string) (*codexRefreshState, error) {
	staged, _, err := readPrivateCodexAuth(filepath.Join(workRoot, ".codex"))
	if err != nil {
		return nil, err
	}
	source, info, err := readPrivateCodexAuth(authHome)
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(staged) != sha256.Sum256(source) {
		return nil, ErrCodexAuthSourceChanged
	}
	identity, identityErr := codexAccountIdentityFromAuth(staged)
	return &codexRefreshState{
		authHome: authHome, workRoot: workRoot, sourceInfo: info,
		sourceDigest: sha256.Sum256(staged), identity: identity, identityOK: identityErr == nil,
	}, nil
}

func (s *codexRefreshState) checkSourceUnchanged() error {
	current, info, err := readPrivateCodexAuth(s.authHome)
	if err != nil || !os.SameFile(s.sourceInfo, info) || sha256.Sum256(current) != s.sourceDigest {
		return ErrCodexAuthSourceChanged
	}
	return nil
}

func (s *codexRefreshState) commit() (retErr error) {
	if err := s.checkSourceUnchanged(); err != nil {
		return err
	}
	candidate, _, err := readPrivateCodexAuth(filepath.Join(s.workRoot, ".codex"))
	if err != nil {
		return err
	}
	if sha256.Sum256(candidate) == s.sourceDigest {
		return nil
	}
	identity, err := codexAccountIdentityFromAuth(candidate)
	if !s.identityOK || err != nil {
		return ErrCodexAuthRefreshUnsafe
	}
	if identity != s.identity {
		return ErrCodexAuthAccountChanged
	}
	temp, err := os.CreateTemp(s.authHome, ".auth-refresh-")
	if err != nil {
		return ErrCodexAuthRefreshUnsafe
	}
	defer func() {
		if err := os.Remove(temp.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, ErrCodexAuthRefreshUnsafe)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return ErrCodexAuthRefreshUnsafe
	}
	_, writeErr := temp.Write(candidate)
	syncErr := temp.Sync()
	closeErr := temp.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return ErrCodexAuthRefreshUnsafe
	}
	if err := s.checkSourceUnchanged(); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), filepath.Join(s.authHome, "auth.json")); err != nil {
		return ErrCodexAuthRefreshUnsafe
	}
	directory, err := os.Open(s.authHome)
	if err != nil {
		return ErrCodexAuthRefreshUnsafe
	}
	syncErr = directory.Sync()
	closeErr = directory.Close()
	if syncErr != nil || closeErr != nil {
		return ErrCodexAuthRefreshUnsafe
	}
	return nil
}
