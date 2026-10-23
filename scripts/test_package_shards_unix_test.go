//go:build !windows

package scripts

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
			blocks := strings.Split(result, "ok\texample.com/shardfixture\n")
			require.GreaterOrEqual(len(blocks), 4, result)
			assert.Contains(blocks[0], "--- PASS: TestPass (")
			assert.Contains(blocks[1], "--- SKIP: TestSkip (")
			assert.NotContains(result, "noisy success sentinel")
			assert.NotContains(result, "skip reason sentinel")
			assert.Contains(result, "    --- SKIP: TestNested/routes/skipped (")
			assert.Contains(result, "ok shard 4: 1 tests")
			if fail == "1" {
				assert.Contains(blocks[2], "=== RUN   TestNested/routes/get")
				assert.Contains(blocks[2], "nested failure sentinel")
				assert.Contains(blocks[2], "--- FAIL: TestNested/routes/get (")
				assert.Contains(blocks[2], "FAIL\texample.com/shardfixture\n")
			} else {
				assert.Len(blocks, 5, result)
				assert.Contains(blocks[2], "        --- PASS: TestNested/routes/get (")
				assert.Contains(blocks[2], "--- PASS: TestNested (")
				assert.NotContains(result, "=== RUN")
				assert.NotContains(result, "FAIL")
			}
		})
	}
}
