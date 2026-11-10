// Package reddit imports extracted Reddit account-data CSV exports.
package reddit

const maxCSVBytes int64 = 256 << 20
const maxEntries = 10000
const batchSize = 100

type ImportOptions struct {
	RootDir string
	Me      string
}
type FileSummary struct {
	Filename                                         string
	Rows, Added, Unchanged, Malformed, MissingIDs    int
	UnknownDates, UnknownSenders, LinksNotDownloaded int
}
type ImportSummary struct {
	SourceID                                         int64
	Files                                            []*FileSummary
	Unsupported                                      []string
	Added, Unchanged, Malformed, MissingIDs          int
	UnknownDates, UnknownSenders, LinksNotDownloaded int
}
