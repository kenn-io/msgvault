package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

// The JSON result must describe durable native runs without stdout progress or
// private input paths. Fresh invocations must expose fresh IDs even on replay.
func TestImportCommandJSONReturnsDurableOpaqueRuns(t *testing.T) {
	for _, format := range []string{"eml", "maildir", "mbox", "pst"} {
		t.Run(format, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			cfg, parent := importCommandCaptureFixture(t, true)
			input := t.TempDir()
			message := []byte("From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <structured-cli@example.net>\r\nSubject: Synthetic result\r\n\r\nSynthetic body\r\n")
			var previous string
			for range 2 {
				var command *cobra.Command
				var args []string
				switch format {
				case "eml", "maildir":
					directory := filepath.Join(input, "Synthetic.mailbox")
					name := "message.eml"
					if format == "maildir" {
						directory = filepath.Join(input, "cur")
						name = "synthetic:2,S"
						for _, part := range []string{"cur", "new", "tmp"} {
							require.NoError(os.MkdirAll(filepath.Join(input, part), 0700))
						}
					}
					require.NoError(os.MkdirAll(directory, 0700))
					require.NoError(os.WriteFile(filepath.Join(directory, name), message, 0600))
					if format == "eml" {
						command = newImportEMLCommand()
					} else {
						command = newImportMaildirCommand()
					}
					args = []string{input, "--identifier", "owner@example.net", "--no-default-identity"}
				case "mbox":
					file := filepath.Join(input, "synthetic.mbox")
					require.NoError(os.WriteFile(file, append([]byte("From sender@example.net Mon Jan 1 00:00:00 2001\n"), message...), 0600))
					command = cloneImportCommandForTest(t, importMboxCmd)
					args = []string{"owner@example.net", file, "--no-default-identity"}
				case "pst":
					file := filepath.Join(input, "synthetic.pst")
					require.NoError(os.WriteFile(file, testutil.PSTFixture(), 0600))
					command = cloneImportCommandForTest(t, importPstCmd)
					args = []string{"owner@example.net", file}
				}
				command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
				command.SetErr(io.Discard)
				command.SetArgs(append(args, "--notify-imports", "--no-resume", "--json"))
				// Capture process stdout too: cache progress currently bypasses Cobra's
				// writer and must not invalidate a machine-readable import result.
				finish := captureStdout(t)
				runErr := command.Execute()
				output := finish()
				require.NoError(runErr)
				var result struct {
					ImportRunIDs []string `json:"import_run_ids"`
					Completed    bool     `json:"completed"`
				}
				require.NoError(json.Unmarshal([]byte(output), &result), "stdout must contain one JSON result")
				require.Len(result.ImportRunIDs, 1)
				assert.True(result.Completed)
				assert.NotEmpty(result.ImportRunIDs[0])
				assert.NotEqual(previous, result.ImportRunIDs[0])
				previous = result.ImportRunIDs[0]
				assert.NotContains(output, input)
				assert.NotContains(output, "owner@example.net")
				assert.NotContains(output, "Synthetic body")
				var frozen bool
				var nativeFormat string
				require.NoError(parent.DB().QueryRow("SELECT notify_imports,import_format FROM import_runs WHERE id=?", previous).Scan(&frozen, &nativeFormat))
				assert.True(frozen)
				assert.Equal(format, nativeFormat)
			}
			var events int
			require.NoError(parent.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'").Scan(&events))
			assert.Equal(1, events)
		})
	}
}

// Automatic Apple Mail account discovery must return all per-source logical
// runs, including accounts whose row was deduplicated on a later invocation.
func TestImportAppleMailJSONReturnsEveryAccountRun(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	cfg, parent := importCommandCaptureFixture(t, true)
	root := t.TempDir()
	accountDB := filepath.Join(t.TempDir(), "Accounts4.sqlite")
	db, err := sql.Open("sqlite3", accountDB)
	require.NoError(err)
	_, err = db.Exec("CREATE TABLE ZACCOUNT (Z_PK INTEGER PRIMARY KEY,ZIDENTIFIER TEXT,ZUSERNAME TEXT,ZACCOUNTDESCRIPTION TEXT,ZPARENTACCOUNT INTEGER)")
	require.NoError(err)
	for i, guid := range []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"} {
		_, err = db.Exec("INSERT INTO ZACCOUNT VALUES (?,?,?,?,NULL)", i+1, guid, fmt.Sprintf("owner%d@example.net", i+1), "Synthetic account")
		require.NoError(err)
		directory := filepath.Join(root, "V10", guid, "Inbox.mbox", "Messages")
		require.NoError(os.MkdirAll(directory, 0700))
		content := fmt.Sprintf("From: sender@example.net\r\nTo: owner%d@example.net\r\nMessage-ID: <account%d@example.net>\r\nSubject: Synthetic account\r\n\r\nSynthetic body\r\n", i+1, i+1)
		require.NoError(os.WriteFile(filepath.Join(directory, "1.emlx"), []byte(fmt.Sprintf("%d\n%s", len(content), content)), 0600))
	}
	require.NoError(db.Close())
	command := cloneImportCommandForTest(t, importEmlxCmd)
	command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	command.SetErr(io.Discard)
	command.SetArgs([]string{root, "--accounts-db", accountDB, "--no-resume", "--no-default-identity", "--notify-imports", "--json"})
	finish := captureStdout(t)
	runErr := command.Execute()
	output := finish()
	require.NoError(runErr)
	var result struct {
		ImportRunIDs []string `json:"import_run_ids"`
		Completed    bool     `json:"completed"`
	}
	require.NoError(json.Unmarshal([]byte(output), &result))
	require.Len(result.ImportRunIDs, 2)
	assert.True(result.Completed)
	rows, err := parent.DB().Query("SELECT id FROM import_runs")
	require.NoError(err)
	defer func() { Require.NoError(t, rows.Close()) }()
	var persisted []string
	for rows.Next() {
		var id string
		require.NoError(rows.Scan(&id))
		persisted = append(persisted, id)
	}
	require.NoError(rows.Err())
	sort.Strings(persisted)
	sort.Strings(result.ImportRunIDs)
	assert.Equal(persisted, result.ImportRunIDs)
	assert.NotContains(output, root)
	assert.NotContains(output, accountDB)
	assert.NotContains(output, "owner1@example.net")
}

func cloneImportCommandForTest(t *testing.T, original *cobra.Command) *cobra.Command {
	t.Helper()
	clone := &cobra.Command{Use: original.Use, Args: original.Args, RunE: original.RunE}
	original.Flags().VisitAll(func(flag *pflag.Flag) {
		changed := flag.Changed
		value := flag.Value.String()
		if slice, ok := flag.Value.(pflag.SliceValue); ok {
			values := append([]string(nil), slice.GetSlice()...)
			t.Cleanup(func() { Require.NoError(t, slice.Replace(values)); flag.Changed = changed })
		} else {
			t.Cleanup(func() { Require.NoError(t, flag.Value.Set(value)); flag.Changed = changed })
		}
	})
	clone.Flags().AddFlagSet(original.Flags())
	return clone
}
