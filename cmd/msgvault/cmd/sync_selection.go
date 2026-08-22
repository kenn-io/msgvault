package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/sourceops"
	"go.kenn.io/msgvault/internal/store"
)

func syncSourceSelector(cmd *cobra.Command, args []string) (sourceops.Selector, bool, error) {
	account := ""
	if len(args) == 1 {
		account = strings.TrimSpace(args[0])
	}
	flag := cmd.Flags().Lookup("source-id")
	if flag == nil || !flag.Changed {
		return sourceops.Selector{Account: account}, account != "", nil
	}
	sourceID, err := cmd.Flags().GetInt64("source-id")
	if err != nil {
		return sourceops.Selector{}, false, fmt.Errorf("read --source-id flag: %w", err)
	}
	switch {
	case sourceID <= 0:
		return sourceops.Selector{}, false, errors.New("source ID must be positive")
	case account != "":
		return sourceops.Selector{}, false, errors.New("account and source ID are mutually exclusive")
	default:
		return sourceops.Selector{SourceID: sourceID, SourceIDSet: true}, true, nil
	}
}

func resolveSyncSources(
	st sourceops.Store,
	selector sourceops.Selector,
) ([]*store.Source, bool, error) {
	selection, err := sourceops.ResolveAllMatches(st, selector)
	if err == nil {
		return selection.Sources, false, nil
	}
	if selector.Account != "" && opserr.KindOf(err) == opserr.KindNotFound {
		return nil, true, nil
	}
	return nil, false, err
}

func syncSelectorLabel(selector sourceops.Selector) string {
	if selector.SourceIDSet {
		return fmt.Sprintf("source ID %d", selector.SourceID)
	}
	return fmt.Sprintf("account %q", selector.Account)
}
