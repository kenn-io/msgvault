package cmd

import (
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestCLIJSONSelector(t *testing.T) {
	command := &cobra.Command{Use: "probe"}
	command.Flags().Bool("json", false, "")
	command.Flags().BoolP("verbose", "v", false, "")
	command.Flags().IntP("count", "n", 0, "")
	command.Flags().String("body", "", "")
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"--nope", "--json"}, true}, {[]string{"--json=false"}, false},
		{[]string{"--json", "--json=false"}, false}, {[]string{"--", "--json"}, false},
		{[]string{"--body", "--json"}, false}, {[]string{"--body=x", "--json"}, true},
		{[]string{"-json"}, false}, {[]string{"---json"}, false}, {[]string{"-vn", "--json"}, false},
	} {
		assert.Equal(t, tc.want, cliJSONRequested(command, tc.args), "%v", tc.args)
	}
}
