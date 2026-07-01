package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
)

func runAggregateListCommand(
	cmd *cobra.Command,
	view query.ViewType,
	emptyMessage string,
	keyHeader string,
	errorLabel string,
) error {
	opts, err := parseCommonFlags()
	if err != nil {
		return err
	}

	engine, cleanup, err := openAggregateQueryEngine(cmd)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer cleanup()

	results, err := engine.Aggregate(cmd.Context(), view, opts)
	if err != nil {
		return query.HintRepairEncoding(fmt.Errorf("aggregate by %s: %w", errorLabel, err))
	}

	if len(results) == 0 {
		fmt.Println(emptyMessage)
		return nil
	}

	if aggJSON {
		return outputAggregateJSON(results)
	}
	outputAggregateTable(results, keyHeader)
	return nil
}

func openAggregateQueryEngine(cmd *cobra.Command) (query.Engine, func(), error) {
	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return nil, func() {}, err
	}
	engine := daemonclient.NewEngineAdapter(st)
	return engine, func() { _ = engine.Close() }, nil
}
