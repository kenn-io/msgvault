package cmd

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

func TestEmbeddingConfirmationAcceptsFinalLine(t *testing.T) {
	command := &cobra.Command{Use: "build"}
	command.SetIn(strings.NewReader("yes"))
	var out bytes.Buffer
	command.SetErr(&out)
	confirmed, err := confirmEmbed(command, "Review? ")
	require.NoError(t, err)
	assert.True(t, confirmed)
}

func TestEmbeddingConfirmationEOFIsUsage(t *testing.T) {
	command := &cobra.Command{Use: "build"}
	command.SetIn(strings.NewReader(""))
	command.SetErr(&bytes.Buffer{})
	confirmed, err := confirmEmbed(command, "Review? ")
	assert.False(t, confirmed)
	require.ErrorIs(t, err, io.EOF)
	var classified *cliError
	require.ErrorAs(t, err, &classified)
	assert.Equal(t, 2, classified.ExitCode)
}
