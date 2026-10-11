package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/atomicfile"
)

const muesliHookMaxBytes = 4096

// decodeMuesliHook accepts the executable launcher's event, not an archive.
func decodeMuesliHook(reader io.Reader) (int64, error) {
	data, err := io.ReadAll(io.LimitReader(reader, muesliHookMaxBytes+1))
	if err != nil || len(data) > muesliHookMaxBytes || !utf8.Valid(data) {
		return 0, errors.New("invalid Muesli completion event")
	}
	var event struct {
		SchemaVersion int    `json:"schemaVersion"`
		Event         string `json:"event"`
		Kind          string `json:"kind"`
		ID            int64  `json:"id"`
		CompletedAt   string `json:"completedAt"`
	}
	if json.Unmarshal(data, &event, json.RejectUnknownMembers(true)) != nil || event.SchemaVersion != 1 || event.Event != "meeting.completed" || event.Kind != "meeting" || event.ID <= 0 {
		return 0, errors.New("invalid Muesli completion event")
	}
	if _, err := time.Parse(time.RFC3339Nano, event.CompletedAt); err != nil {
		return 0, errors.New("invalid Muesli completion event")
	}
	return event.ID, nil
}

var muesliHookInstall string
var muesliHookCmd = &cobra.Command{
	Use: "muesli-hook", Short: "Sync the meeting named by a Muesli completion event", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if muesliHookInstall != "" {
			path, err := installMuesliHook(muesliHookInstall)
			if err == nil {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), path)
			}
			return err
		}
		id, err := decodeMuesliHook(cmd.InOrStdin())
		if err != nil {
			return err
		}
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		sources, err := muesliSources(state.cfg).selected(nil)
		if err != nil {
			return err
		}
		if len(sources) != 1 {
			return errors.New("muesli completion hook requires exactly one configured source")
		}
		if isRemoteModeFor(state) {
			return runMuesliClientSync(cmd, nil, id)
		}
		return runDaemonCLICommandHTTPWithEnv(cmd, []string{"sync-muesli", sources[0].Identifier, "--meeting-id", strconv.FormatInt(id, 10)}, nil, false, false)
	},
}

// installMuesliHook links the hook name to the msgvault the user invoked. It
// keeps a package manager's stable path, such as Homebrew's bin symlink, so an
// upgrade that removes the versioned file does not break the hook. Reinstalling
// replaces an earlier hook link but never another file.
func installMuesliHook(dir string) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("install Muesli hook: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("install Muesli hook: %w", err)
	}
	path, err := filepath.Abs(filepath.Join(dir, "msgvault-muesli-hook"))
	if err != nil {
		return "", fmt.Errorf("install Muesli hook: %w", err)
	}
	if strings.EqualFold(filepath.Ext(executable), ".exe") {
		path += ".exe"
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("install Muesli hook: %s exists and is not a hook link; move it first", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("install Muesli hook: %w", err)
	}
	target := muesliHookTarget(executable, path)
	if hookEntry(target) == hookEntry(path) {
		return "", fmt.Errorf("install Muesli hook: run the install from msgvault, not from %s", path)
	}
	staged := filepath.Join(filepath.Dir(path), fmt.Sprintf(".msgvault-muesli-hook-%d.tmp", os.Getpid()))
	if err := os.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("install Muesli hook: %w", err)
	}
	if err := os.Symlink(target, staged); err != nil {
		return "", fmt.Errorf("install Muesli hook: %w", err)
	}
	if err := atomicfile.Replace(staged, path); err != nil {
		return "", errors.Join(fmt.Errorf("install Muesli hook: %w", err), os.Remove(staged))
	}
	return path, nil
}

// muesliHookTarget prefers the invoked path over os.Executable, which resolves
// symlinks on some platforms, when both name the same file. Reinstalling by
// running the hook itself keeps the hook's current target.
func muesliHookTarget(executable, hook string) string {
	invoked, err := exec.LookPath(os.Args[0])
	if err != nil {
		return executable
	}
	invoked, err = filepath.Abs(invoked)
	if err != nil {
		return executable
	}
	if hookEntry(invoked) == hookEntry(hook) {
		if current, err := os.Readlink(hook); err == nil {
			if !filepath.IsAbs(current) {
				current = filepath.Join(filepath.Dir(hook), current)
			}
			return current
		}
		return executable
	}
	// #nosec G703 -- argv[0] is the user's own command; it is only compared
	// with the running executable, never read or written.
	invokedInfo, invokedErr := os.Stat(invoked)
	runningInfo, runningErr := os.Stat(executable)
	if invokedErr == nil && runningErr == nil && os.SameFile(invokedInfo, runningInfo) {
		return invoked
	}
	return executable
}

// hookEntry names a directory entry with its parent directory's symlinks
// resolved, so two spellings of the same hook path compare equal.
func hookEntry(path string) string {
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Join(dir, filepath.Base(path))
}

func init() {
	muesliHookCmd.Flags().StringVar(&muesliHookInstall, "install", "", "create the native executable symlink in this directory")
	rootCmd.AddCommand(muesliHookCmd)
}
