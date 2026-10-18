package cmd

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/store"
)

func runExportEMLFamily(cmd *cobra.Command, messageRef, outputPath string) error {
	id, err := strconv.ParseInt(messageRef, 10, 64)
	if err != nil || id < 1 {
		return errors.New("--family requires a positive archive message ID")
	}
	client, _, err := OpenHTTPStore(cmd.Context(), "3.26.0")
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	family, err := client.EmailFamilyContext(cmd.Context(), id, store.EmailFamilyOptions{})
	if err != nil {
		return err
	}
	if outputPath == stdoutSentinel {
		return client.ExportEmailFamilyContext(cmd.Context(), id, family.Revision, store.EmailFamilyOptions{}, cmd.OutOrStdout())
	}
	if outputPath == "" {
		outputPath = fmt.Sprintf("email-family-%d.zip", id)
	}
	staged, err := atomicfile.Create(outputPath, atomicfile.WithPerm(emlFileMode))
	if err != nil {
		return err
	}
	defer func() { _ = staged.Abort() }()
	if err := fileutil.SecureChmod(staged.TempName(), emlFileMode); err != nil {
		return err
	}
	if err := client.ExportEmailFamilyContext(cmd.Context(), id, family.Revision, store.EmailFamilyOptions{}, staged); err != nil {
		return err
	}
	if err := staged.Commit(); err != nil {
		return err
	}
	cmd.Printf("Exported archived family to: %s\n", outputPath)
	return nil
}
