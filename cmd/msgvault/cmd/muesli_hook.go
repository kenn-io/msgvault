package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
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
		sources, err := resolveMuesliSources(nil, state.cfg)
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

func installMuesliHook(dir string) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "msgvault-muesli-hook")
	if err := os.Symlink(executable, path); err != nil {
		return "", fmt.Errorf("install Muesli hook: %w", err)
	}
	return path, nil
}
func init() {
	muesliHookCmd.Flags().StringVar(&muesliHookInstall, "install", "", "create the native executable symlink in this directory")
	rootCmd.AddCommand(muesliHookCmd)
}
