package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
)

// Native parser writes are covered separately. The local HTTP runner is a
// protocol peer here: the actual public command must forward the parsed choice
// and preserve its daemon's structured stdout without synthesizing another run.
func TestImportCommandForwardsChoicesAndJSONThroughDaemonHTTP(t *testing.T) {
	for _, notify := range []bool{false, true} {
		t.Run(strconv.FormatBool(notify), func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			t.Setenv(daemonCLISubprocessEnv, "")
			payload := `{"import_run_ids":["import_synthetic-wire"],"completed":true}` + "\n"
			frame, err := json.Marshal(map[string]string{"type": "stdout", "data": payload})
			require.NoError(err)
			server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
				choice := "--notify-imports"
				if !notify {
					choice += "=false"
				}
				Assert.Equal(t, []string{"import-synctech-sms", "--json", choice, "--owner-phone=+15550001000", "synthetic.xml"}, req.Args)
				Assert.Empty(t, req.Env, "client arguments cannot supply the daemon's capture snapshot")
			}, string(frame), `{"type":"complete"}`)
			command := newImportSynctechSMSCmd()
			command.SetContext(configureRemoteDaemonForTest(t, server.URL))
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(io.Discard)
			command.SetArgs([]string{"synthetic.xml", "--owner-phone", "+15550001000", "--notify-imports=" + strconv.FormatBool(notify), "--json"})
			require.NoError(command.Execute())
			assert.EqualValues(1, requests.Load())
			assert.Equal(payload, output.String())
		})
	}
}
