//go:build !windows

package scripts

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPackageShardsUnix(t *testing.T) {
	r := require.New(t)
	script, err := filepath.Abs("test-package-shards.sh")
	r.NoError(err)
	// This read makes Go's test cache rerun the test when the child's inputs change.
	_, err = os.ReadFile(script)
	r.NoError(err)
	bash, err := exec.LookPath("bash")
	r.NoError(err)
	dir := filepath.Join(t.TempDir(), "package with spaces")
	r.NoError(os.Mkdir(dir, 0o700))
	r.NoError(os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/shardfixture\n\ngo 1.25\n"), 0o600))
	r.NoError(os.WriteFile(filepath.Join(dir, "fixture_test.go"), []byte(`package shardfixture
import ("flag"; "fmt"; "os"; "testing")
func TestMain(m *testing.M) {
    flag.Parse()
    // A successful shard may produce no named outcome records.
    if flag.Lookup("test.run").Value.String() == "^(TestSilent)$" { os.Exit(0) }
    os.Exit(m.Run())
}
func TestPass(t *testing.T) { fmt.Println("noisy success sentinel") }
func TestSkip(t *testing.T) { t.Skip("skip reason sentinel") }
func TestNested(t *testing.T) {
    t.Run("routes", func(t *testing.T) {
        t.Run("get", func(t *testing.T) {
            if os.Getenv("SHARD_FAIL") == "1" { t.Log("nested failure sentinel"); t.Fail() }
        })
        t.Run("skipped", func(t *testing.T) { t.Skip("nested skip reason") })
    })
}
func TestSilent(t *testing.T) {}
`), 0o600))
	for _, fail := range []string{"1", "0"} {
		t.Run("SHARD_FAIL="+fail, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			t.Setenv("SHARD_FAIL", fail)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, bash, script, ".", "4", "fts5 sqlite_vec", "30s")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOWORK=off")
			output, err := cmd.CombinedOutput()
			result := string(output)
			if fail == "1" {
				require.Error(err, result)
			} else {
				require.NoError(err, result)
			}
			// Each shard ends with the package line go test prints after a package's results.
			blocks := regexp.MustCompile(`(?m)^ok  \texample\.com/shardfixture\t\d+\.\d{3}s\n`).Split(result, -1)
			require.GreaterOrEqual(len(blocks), 4, result)
			assert.Contains(blocks[0], "--- PASS: TestPass (")
			assert.Contains(blocks[1], "--- SKIP: TestSkip (")
			assert.NotContains(result, "noisy success sentinel")
			assert.NotContains(result, "skip reason sentinel")
			assert.Contains(result, "--- SKIP: TestNested/routes/skipped (")
			assert.Contains(result, "ok shard 4: 1 tests")
			if fail == "1" {
				assert.Contains(blocks[2], "=== RUN   TestNested/routes/get")
				assert.Contains(blocks[2], "nested failure sentinel")
				assert.Contains(blocks[2], "--- FAIL: TestNested/routes/get (")
				assert.Regexp(`(?m)^FAIL\texample\.com/shardfixture\t\d+\.\d{3}s$`, blocks[2])
			} else {
				assert.Len(blocks, 5, result)
				assert.Contains(blocks[2], "--- PASS: TestNested/routes/get (")
				assert.Contains(blocks[2], "--- PASS: TestNested (")
				assert.NotContains(result, "=== RUN")
				assert.NotContains(result, "FAIL")
			}

			// Every shard's test2json events land in one file named after the package.
			events, err := os.ReadFile(filepath.Join(dir, "gotestsum", "shardfixture.json"))
			require.NoError(err)
			outcomes := map[string]string{}
			packageResults := 0
			for line := range strings.Lines(string(events)) {
				var event struct {
					Action  string `json:"Action"`
					Package string `json:"Package"`
					Test    string `json:"Test"`
				}
				require.NoError(json.Unmarshal([]byte(line), &event), line)
				require.Equal("example.com/shardfixture", event.Package, line)
				switch event.Action {
				case "pass", "fail", "skip":
					if event.Test == "" {
						packageResults++
					} else {
						outcomes[event.Test] = event.Action
					}
				}
			}
			assert.Equal(4, packageResults, "one package result per shard")
			wantGet := "pass"
			if fail == "1" {
				wantGet = "fail"
			}
			assert.Equal(map[string]string{
				"TestPass":                  "pass",
				"TestSkip":                  "skip",
				"TestNested":                wantGet,
				"TestNested/routes":         wantGet,
				"TestNested/routes/get":     wantGet,
				"TestNested/routes/skipped": "skip",
			}, outcomes)
		})
	}
}
