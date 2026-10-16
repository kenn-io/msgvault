package cmd

import (
	"errors"
	"fmt"
	"slices"
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

func rejectExplicitlyRetiredSyncSources(
	sources []*store.Source,
	selector sourceops.Selector,
	isSyncable func(*store.Source) bool,
) error {
	if slices.ContainsFunc(activeSyncSources(sources), isSyncable) {
		return nil
	}
	for _, source := range sources {
		if source.MergedIntoSourceID == 0 {
			continue
		}
		switch source.SourceType {
		case sourceTypeGmail, sourceTypeIMAP, sourceTypeMSMail, "":
			return fmt.Errorf("%s is retired: %w", syncSelectorLabel(selector), store.ErrSourceRetired)
		}
	}
	return nil
}

func isIncrementalSyncableSource(source *store.Source) bool {
	switch source.SourceType {
	case sourceTypeGmail, sourceTypeIMAP, sourceTypeMSMail, "":
		return true
	default:
		return false
	}
}

func isFullSyncableSource(source *store.Source) bool {
	switch source.SourceType {
	case sourceTypeGmail, sourceTypeIMAP, "":
		return true
	default:
		return false
	}
}

func activeSyncSources(sources []*store.Source) []*store.Source {
	active := make([]*store.Source, 0, len(sources))
	for _, source := range sources {
		if source.MergedIntoSourceID == 0 {
			active = append(active, source)
		}
	}
	return active
}

func syncSelectorLabel(selector sourceops.Selector) string {
	if selector.SourceIDSet {
		return fmt.Sprintf("source ID %d", selector.SourceID)
	}
	return fmt.Sprintf("account %q", selector.Account)
}
