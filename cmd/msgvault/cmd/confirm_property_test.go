//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package cmd

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Hegel vendors native engines only for these platforms; deterministic prompt
// regressions remain in confirm_test.go for every supported CLI platform.
func TestConfirmationAnswersMatchTheirMode(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		answer := hegel.Draw(ht, hegel.Text())
		mode := hegel.Draw(ht, hegel.SampledFrom([]ConfirmMode{ConfirmModePermanent, ConfirmModeAllHidden, ConfirmModeYesNo}))
		// Scanner reads one line. A newline terminates the answer regardless of later data.
		line, _, _ := strings.Cut(answer, "\n")
		var out bytes.Buffer
		got, err := confirmDestructive(strings.NewReader(answer+"\n"), &out, mode)
		if len(line) >= bufio.MaxScanTokenSize {
			require.ErrorContains(ht, err, "read confirmation:")
			assert.False(ht, got)
			return
		}
		require.NoError(ht, err)
		want := false
		switch strings.TrimSpace(line) {
		case "y", "Y", "yes", "yeS", "yEs", "yES", "Yes", "YeS", "YEs", "YES":
			want = true
		}
		if mode == ConfirmModePermanent {
			want = strings.TrimSpace(line) == "delete"
		}
		assert.Equal(ht, want, got)
	}, hegel.WithDatabase(""))
}

func TestConfirmationLineEndingsAgreeProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		answer := hegel.Draw(ht, hegel.SampledFrom([]string{"y", "yes", "Y", "YES", "no", "n", "delete", "other"}))
		padding := hegel.Draw(ht, hegel.SampledFrom([]string{"", " ", "\t"}))
		line := padding + answer + padding
		for _, ending := range []string{"", "\n", "\r\n"} {
			got, err := readYesNoConfirmation(bufio.NewReader(strings.NewReader(line + ending)))
			require.NoError(ht, err)
			assert.Equal(ht, isYesAnswer(strings.ToLower(answer)), got)
		}
	}, hegel.WithDatabase(t.TempDir()))
}
