//go:build linux

package peoplesweep

import (
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"errors"
	"io"
	"os"
	"path/filepath"
)

func (l codexBoundLauncher) StartLogin(ctx context.Context, attestation CodexAttestation, authHome string) (RPCProcess, error) {
	if err := codexPrivateAuthHome(authHome); err != nil {
		return nil, err
	}
	if l.proxy == nil {
		return nil, ErrCodexProxyUnreleased
	}
	if _, err := os.Lstat(filepath.Join(authHome, "auth.json")); err == nil {
		contents, _, err := readPrivateCodexAuth(authHome)
		if err != nil {
			return nil, err
		}
		if _, err := codexAccountIdentityFromAuth(contents); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("codex dedicated auth.json cannot be inspected")
	}
	return l.start(ctx, attestation, "", true)
}

// CommitLoginAuth is called only after the exact successful device-login
// notification, before the disposable process root is removed. A repeat login
// atomically replaces a private dedicated credential after identity checks.
func (p *codexOwnedProcess) CommitLoginAuth(authHome string) error {
	if err := codexPrivateAuthHome(authHome); err != nil {
		return err
	}
	if !filepath.IsAbs(p.workRoot) || filepath.Clean(p.workRoot) != p.workRoot {
		return errors.New("codex login work root is invalid")
	}
	workInfo, err := os.Lstat(p.workRoot)
	if err != nil || !workInfo.IsDir() || workInfo.Mode().Perm()&0o077 != 0 || !codexAuthOwnedByDaemon(workInfo) {
		return errors.New("codex login work root is unavailable")
	}
	stagedInfo, err := os.Lstat(filepath.Join(p.workRoot, ".codex"))
	if err != nil || !stagedInfo.IsDir() || stagedInfo.Mode().Perm()&0o077 != 0 || !codexAuthOwnedByDaemon(stagedInfo) {
		return errors.New("codex login credential directory is unavailable")
	}
	root, err := os.OpenRoot(filepath.Join(p.workRoot, ".codex"))
	if err != nil {
		return errors.New("open codex login credential directory")
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat("auth.json")
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 ||
		info.Size() <= 0 || info.Size() > 1<<20 || !codexAuthOwnedByDaemon(info) {
		return errors.New("codex login auth.json must be a private regular file under 1 MiB")
	}
	source, err := root.Open("auth.json")
	if err != nil {
		return errors.New("open codex login auth.json")
	}
	defer func() { _ = source.Close() }()
	opened, err := source.Stat()
	if err != nil || !os.SameFile(info, opened) || !codexAuthOwnedByDaemon(opened) {
		return errors.New("codex login auth.json changed during open")
	}
	contents, err := io.ReadAll(io.LimitReader(source, 1<<20+1))
	if err != nil || len(contents) == 0 || len(contents) > 1<<20 || !jsontext.Value(contents).IsValid() {
		return errors.New("codex login auth.json is invalid")
	}
	finalInfo, err := source.Stat()
	if err != nil || !os.SameFile(opened, finalInfo) || finalInfo.Size() != int64(len(contents)) ||
		!finalInfo.ModTime().Equal(opened.ModTime()) {
		return errors.New("codex login auth.json changed during read")
	}
	if _, err := codexAccountIdentityFromAuth(contents); err != nil {
		return err
	}
	destination := filepath.Join(authHome, "auth.json")
	var previous []byte
	var previousInfo os.FileInfo
	if _, err := os.Lstat(destination); err == nil {
		previous, previousInfo, err = readPrivateCodexAuth(authHome)
		if err != nil {
			return err
		}
		if _, err := codexAccountIdentityFromAuth(previous); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("codex dedicated auth.json cannot be inspected")
	}
	temp, err := os.CreateTemp(authHome, ".auth-draft-")
	if err != nil {
		return errors.New("create private codex login draft")
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return errors.New("secure codex login draft")
	}
	_, writeErr := temp.Write(contents)
	syncErr := temp.Sync()
	closeErr := temp.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("write codex login draft")
	}
	if previousInfo == nil {
		if err := os.Link(temp.Name(), destination); err != nil {
			return errors.New("commit codex login auth.json")
		}
	} else {
		current, currentInfo, err := readPrivateCodexAuth(authHome)
		if err != nil || !os.SameFile(previousInfo, currentInfo) || sha256.Sum256(previous) != sha256.Sum256(current) {
			return ErrCodexAuthSourceChanged
		}
		if err := os.Rename(temp.Name(), destination); err != nil {
			return errors.New("replace codex login auth.json")
		}
	}
	rollback := func(cause error) error {
		return errors.Join(cause, restorePreviousCodexAuth(authHome, previous, previousInfo))
	}
	if err := os.Remove(temp.Name()); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return rollback(errors.New("remove codex login draft after commit"))
		}
	}
	directory, err := os.Open(authHome)
	if err != nil {
		return rollback(errors.New("open codex dedicated auth home after commit"))
	}
	syncErr = directory.Sync()
	closeErr = directory.Close()
	if syncErr != nil || closeErr != nil {
		return rollback(errors.New("sync codex dedicated auth home"))
	}
	return nil
}

func restorePreviousCodexAuth(authHome string, previous []byte, previousInfo os.FileInfo) error {
	destination := filepath.Join(authHome, "auth.json")
	if previousInfo == nil {
		return os.Remove(destination)
	}
	temp, err := os.CreateTemp(authHome, ".auth-rollback-")
	if err != nil {
		return errors.New("restore previous codex auth.json")
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	_, writeErr := temp.Write(previous)
	modeErr := temp.Chmod(previousInfo.Mode().Perm())
	syncErr := temp.Sync()
	closeErr := temp.Close()
	if writeErr != nil || modeErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("restore previous codex auth.json")
	}
	if err := os.Rename(temp.Name(), destination); err != nil {
		return errors.New("restore previous codex auth.json")
	}
	directory, err := os.Open(authHome)
	if err != nil {
		return errors.New("sync restored codex auth.json")
	}
	syncErr = directory.Sync()
	closeErr = directory.Close()
	if syncErr != nil || closeErr != nil {
		return errors.New("sync restored codex auth.json")
	}
	return nil
}

func codexPrivateAuthHome(authHome string) error {
	if !filepath.IsAbs(authHome) || filepath.Clean(authHome) != authHome {
		return errors.New("codex dedicated auth home must be an absolute clean path")
	}
	info, err := os.Lstat(authHome) //nolint:gosec // The daemon selects this absolute, clean path; ownership and mode are checked below.
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !codexAuthOwnedByDaemon(info) {
		return errors.New("codex dedicated auth home must be private and daemon-owned")
	}
	return nil
}
