// Package reddit imports extracted Reddit account-data CSV exports.
package reddit

const batchSize = 100

type ImportOptions struct {
	RootDir string
	Me      string
}
type FileSummary struct {
	Filename                                      string
	Rows, Added, Unchanged, Malformed, MissingIDs int
	UnknownDates, UnknownSenders                  int
}
type ImportSummary struct {
	SourceID                                int64
	Files                                   []*FileSummary
	Unsupported                             []string
	Added, Unchanged, Malformed, MissingIDs int
	UnknownDates, UnknownSenders            int
}
