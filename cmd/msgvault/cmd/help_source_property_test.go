//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package cmd

import (
	"errors"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
	"strings"
	"testing"
)

func TestSourceNotConfiguredPreservesCauseProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		message := hegel.Draw(ht, hegel.Text())
		provider := hegel.Draw(ht, hegel.SampledFrom([]string{"gmail", "imap", "o365", "teams", "calendar", "matrix", "synctech-sms", "granola", "omi", "circleback", "muesli", "twenty", "notion-meetings", "plaud", "twilio", "chatwoot", "inline", "slack", "discord", "beeper"}))
		cause := errors.New(message)
		err := sourceNotConfigured(provider, cause)
		assert.Equal(ht, message, err.Error())
		require.ErrorIs(ht, err, cause)
	})
}

func TestSourceImportArityProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		args := hegel.Draw(ht, hegel.Lists(hegel.Text()))
		for _, command := range []*cobra.Command{importMboxCmd, importPstCmd} {
			err := command.Args(command, args)
			if len(args) == 2 {
				require.NoError(ht, err)
			} else if assert.Error(ht, err) {
				assert.Contains(ht, err.Error(), "<identifier>")
			}
		}
	})
}

func TestSourceRequiredStringAcceptanceProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		value := hegel.Draw(ht, hegel.OneOf[string](hegel.Text(), hegel.SampledFrom([]string{"", "\t\n", "\u00a0\u2003"})))
		inherited := hegel.Draw(ht, hegel.Booleans())
		root := &cobra.Command{Use: "msgvault"}
		command := &cobra.Command{Use: "import-eml <mail-dir>"}
		root.AddCommand(command)
		if inherited {
			root.PersistentFlags().String("identifier", value, "archive identifier")
		} else {
			command.Flags().String("identifier", value, "archive identifier")
		}
		require.NoError(ht, command.ParseFlags(nil))
		err := sourceRequiredString(command, "identifier", "archive account", "msgvault import eml ./mail --identifier you@example.com")
		if strings.TrimSpace(value) == "" {
			if assert.Error(ht, err) {
				assert.Equal(ht, 2, ExitCode(err))
				assert.Contains(ht, classifyCLIError(command, err).Hint, "--identifier you@example.com")
			}
		} else {
			assert.NoError(ht, err)
		}
	})
}

func TestSourceRequiredStringDoesNotMutateFlagsProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		value := hegel.Draw(ht, hegel.Text())
		changed := hegel.Draw(ht, hegel.Booleans())
		inherited := hegel.Draw(ht, hegel.Booleans())
		root := &cobra.Command{Use: "msgvault"}
		command := &cobra.Command{Use: "example"}
		root.AddCommand(command)
		if inherited {
			root.PersistentFlags().String("identifier", value, "archive identifier")
		} else {
			command.Flags().String("identifier", value, "archive identifier")
		}
		require.NoError(ht, command.ParseFlags(nil))
		flag := command.Flags().Lookup("identifier")
		flag.Changed = changed
		_ = sourceRequiredString(command, "identifier", "archive account", "msgvault account list --json")
		assert.Equal(ht, value, flag.Value.String())
		assert.Equal(ht, changed, flag.Changed)
	})
}

func TestSourceImportArgsChecksArityBeforeRequiredFlagProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		args := hegel.Draw(ht, hegel.Lists(hegel.Text()))
		command := &cobra.Command{Use: "import messenger <dyi-export-dir>"}
		command.Flags().String("me", "", "your Messenger identifier")
		err := importMessengerCmd.Args(command, args)
		require.Error(ht, err)
		if len(args) != 1 {
			assert.Equal(ht, 2, ExitCode(err))
			assert.Contains(ht, err.Error(), "<dyi-export-dir>")
		} else {
			assert.Equal(ht, 2, ExitCode(err))
			assert.Contains(ht, err.Error(), "--me is required")
			assert.Contains(ht, classifyCLIError(command, err).Hint, "msgvault import messenger")
		}
	})
}
