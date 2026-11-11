package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runPostalCLI(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "msgvault"}
	person := &cobra.Command{Use: personValue}
	person.AddCommand(newPersonAddressCommand())
	root.AddCommand(person)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{personValue, "address"}, args...))
	err := root.ExecuteContext(ctx)
	return out.String(), err
}
func postalCLIInput(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "postal.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"address_kind":"postal","street_address":" 12 Example St ","extended_address":"Unit 7","country_code":"ca","original_value":" raw text "}`), 0o600))
	return path
}

func TestPostalAddressCLIAssessmentExplicitConsent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	for _, external := range []bool{false, true} {
		daemon := newPersonBriefTestDaemon(t, `{"original":{"address_kind":"postal","street_address":" 12 Example St "},"proposed":{"address_kind":"postal","street_address":"12 Example St"},"status":"normalized_unverified","reason_codes":["not_verified"],"original_extended_parts":[],"proposed_extended_parts":[]}`)
		args := []string{"assess", "7", "--input", postalCLIInput(t)}
		if external {
			args = append(args, "--external")
		}
		output, err := runPostalCLI(daemon.ctx, t, args...)
		require.NoError(err, output)
		assert.Equal("POST", daemon.method)
		assert.Equal("/api/v1/people/7/addresses/assess", daemon.path)
		var body struct {
			AllowExternal bool `json:"allow_external"`
			Address       struct {
				Street string `json:"street_address"`
			} `json:"address"`
		}
		require.NoError(json.Unmarshal([]byte(daemon.body), &body))
		assert.Equal(external, body.AllowExternal)
		assert.Equal(" 12 Example St ", body.Address.Street)
		assert.Contains(output, "normalized_unverified")
	}
}

func TestPostalAddressCLIApplyRequiresExplicitRevisionAndTarget(t *testing.T) {
	daemon := newPersonBriefTestDaemon(t, `{"person":{"id":7,"vcard_uid":"person-7","participant_ids":[],"revision":5},"addresses":[],"contact_points":[],"names":[],"dates":[],"categories":[],"media":[]}`)
	input := postalCLIInput(t)
	require := require.New(t)
	for _, flags := range [][]string{{}, {"--add"}, {"--revision", "4"}, {"--revision", "4", "--add", "--supersede", "9"}, {"--revision", "4", "--supersede", "-1"}} {
		args := append([]string{"apply", "7", "--input", input}, flags...)
		_, err := runPostalCLI(daemon.ctx, t, args...)
		require.Error(err)
	}
	assert := assert.New(t)
	assert.Zero(daemon.requests.Load())
	for _, target := range [][]string{{"--add"}, {"--supersede", "9"}} {
		args := append([]string{"apply", "7", "--input", input, "--revision", "4"}, target...)
		output, err := runPostalCLI(daemon.ctx, t, args...)
		require.NoError(err, output)
		assert.Equal("PATCH", daemon.method)
		assert.Equal(`"person-7-r4"`, daemon.ifMatch)
		assert.Equal("/api/v1/people/7/profile", daemon.path)
		var body struct {
			Addresses struct {
				Add []struct {
					Envelope struct {
						Source string `json:"source"`
					} `json:"envelope"`
				} `json:"add"`
				Supersede []int64 `json:"supersede"`
			} `json:"addresses"`
		}
		require.NoError(json.Unmarshal([]byte(daemon.body), &body))
		require.Len(body.Addresses.Add, 1)
		assert.Equal("user", body.Addresses.Add[0].Envelope.Source)
		if target[0] == "--supersede" {
			assert.Equal([]int64{9}, body.Addresses.Supersede)
		} else {
			assert.Empty(body.Addresses.Supersede)
		}
	}
}

func TestPostalAddressCLIListCarriesAssessment(t *testing.T) {
	daemon := newPersonBriefTestDaemon(t, `{"person":{"id":7,"vcard_uid":"person-7","participant_ids":[],"revision":5},"addresses":[{"person_id":7,"address_kind":"postal","street_address":"12 Example Street","original_value":"raw","assessment":{"original":{"address_kind":"postal","original_value":"raw"},"proposed":{"address_kind":"postal","street_address":"12 Example Street"},"status":"needs_review","reason_codes":["not_verified","country_missing"],"original_extended_parts":[],"proposed_extended_parts":[]},"envelope":{"id":1,"source":"user","ordinal":0,"created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"}}],"contact_points":[],"names":[],"dates":[],"categories":[],"media":[]}`)
	output, err := runPostalCLI(daemon.ctx, t, "list", "7")
	require.NoError(t, err, output)
	assert := assert.New(t)
	assert.Equal("GET", daemon.method)
	assert.Equal("/api/v1/people/7/profile", daemon.path)
	assert.Contains(output, `"status":"needs_review"`)
}

func TestPostalAddressCLIRejectsOversizedInput(t *testing.T) {
	daemon := newPersonBriefTestDaemon(t, `{}`)
	path := filepath.Join(t.TempDir(), "postal.json")
	value := `{"address_kind":"postal","street_address":"12 Example St"}`
	input := value + strings.Repeat(" ", (1<<20)-len(value)) + ` {}`
	require.NoError(t, os.WriteFile(path, []byte(input), 0o600))
	_, err := runPostalCLI(daemon.ctx, t, "apply", "7", "--input", path, "--revision", "4", "--add")
	require.Error(t, err)
	assert.Zero(t, daemon.requests.Load())
	require.NoError(t, os.WriteFile(path, []byte(value+strings.Repeat(" ", (1<<20)-len(value))), 0o600))
	_, err = runPostalCLI(daemon.ctx, t, "apply", "7", "--input", path, "--revision", "4", "--add")
	require.NoError(t, err)
	assert.EqualValues(t, 1, daemon.requests.Load())
}

func TestPostalAddressCLIRejectsNullInput(t *testing.T) {
	daemon := newPersonBriefTestDaemon(t, `{}`)
	path := filepath.Join(t.TempDir(), "postal.json")
	require.NoError(t, os.WriteFile(path, []byte(" \nnull \n"), 0o600))
	for _, args := range [][]string{{"assess", "7", "--input", path}, {"apply", "7", "--input", path, "--revision", "4", "--add"}} {
		_, err := runPostalCLI(daemon.ctx, t, args...)
		require.ErrorContains(t, err, "address input must be a JSON object")
	}
	assert.Zero(t, daemon.requests.Load())
}
