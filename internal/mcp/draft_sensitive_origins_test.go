package mcp

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/msgsegment"
)

func TestDraftSensitiveOrigins(t *testing.T) {
	for _, tc := range []struct {
		name, body, subject string
		origins             []string
	}{
		{"unmarked", "password: synthetic-password", "", []string{"authored"}},
		{"angle quote", "> password: synthetic-password", "", []string{"quoted"}},
		{"unicode and CRLF", "Hello 世界\r\n>> password: synthetic-password\r\n", "", []string{"quoted"}},
		{"attribution", "On Friday, Example wrote:\n> password: synthetic-password", "", []string{"quoted"}},
		{"forward", "Note\n----- Forwarded message -----\nFrom: sender@example.com\n\npassword: synthetic-password", "", []string{"quoted"}},
		{"mixed", "password: authored-password\n> password: quoted-password\npassword: another-password", "", []string{"authored", "quoted", "authored"}},
		{"finding crosses origins", "password:\n>synthetic-password", "", []string{"authored"}},
		{"signature", "Thanks\n-- \npassword: synthetic-password", "", []string{"authored"}},
		{"code fence", "```text\n> password: synthetic-password\n```", "", []string{"authored"}},
		{"subject stays authored", "> password: quoted-password", "password: subject-password", []string{"quoted", "authored"}},
		{"unexamined tail", strings.Repeat("x", msgsegment.MaxBodyBytes) + "\n> password: synthetic-password", "", []string{"authored"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings := draftSensitiveFindings("draft-compose", map[string]any{"body": tc.body, "subject": tc.subject})
			require.Len(t, findings, len(tc.origins))
			for i, finding := range findings {
				assert.Equal(t, "password", finding.Kind)
				assert.Equal(t, tc.origins[i], finding.Origin)
			}
		})
	}
}

func TestQuotedSensitiveDraftReceipts(t *testing.T) {
	for _, command := range []string{"draft-compose", "draft-reply", "draft-forward", "draft-edit"} {
		t.Run(command, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			body := "> password: synthetic-password"
			runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"status":"created","draft_id":"d1"}`}}
			args := map[string]any{"body": body}
			switch command {
			case "draft-compose":
				args["to"] = []string{"recipient@example.com"}
			case "draft-edit":
				args["draft_id"], args["revision"] = "d1", 2
			default:
				args["message_id"] = 7
			}
			result := rawCallTool(t, ServeOptions{Drafts: runner, DraftCommands: []string{command}}, strings.ReplaceAll(command, "-", "_"), args)
			assert.NotEqual(true, result["isError"])
			assert.Equal(1, runner.calls)
			assert.Equal([]string{body}, runner.request.Flags["body"])
			structured, ok := result["structuredContent"].(map[string]any)
			require.True(ok)
			warnings, ok := structured["warnings"].([]any)
			require.True(ok)
			require.Len(warnings, 1)
			warning, ok := warnings[0].(map[string]any)
			require.True(ok)
			assert.Equal("quoted", warning["origin"])
			assert.Equal("sensitive_content", warning["code"])
			assert.Contains(warning["message"], "Quoted or forwarded")
			encoded, err := json.Marshal(result)
			require.NoError(err)
			assert.NotContains(string(encoded), "synthetic-password")
			assert.NotContains(warning, "start")
			assert.NotContains(warning, "end")
		})
	}
}

func TestDraftSensitiveOriginsOverlappingFindings(t *testing.T) {
	body := "password: 4111111111111111\n> password: sk-" + strings.Repeat("synthetic", 4)
	findings := draftSensitiveFindings("draft-compose", map[string]any{"body": body})
	require.Len(t, findings, 4)
	for i, origin := range []string{"authored", "authored", "quoted", "quoted"} {
		assert.Equal(t, origin, findings[i].Origin)
	}
}

func BenchmarkDraftSensitiveOriginsManySegments(b *testing.B) {
	for _, tc := range []struct {
		name  string
		pairs int
	}{
		{"256pairs", 256},
		{"4096pairs", 4096},
		{"16384pairs", 16384},
	} {
		b.Run(tc.name, func(b *testing.B) {
			body := strings.Repeat("password: synthetic-password\n> password: synthetic-password\n", tc.pairs)
			args := map[string]any{"body": body}
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				draftSensitiveFindings("draft-compose", args)
			}
		})
	}
}
