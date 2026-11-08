package clirun

import (
	"io"

	"github.com/spf13/pflag"
)

// AddLoggingFlags declares the root logging flags. The CLI forwards these
// after the command words when it runs a command through the daemon, so the
// root command and the daemon's argument checks share this definition.
func AddLoggingFlags(flags *pflag.FlagSet) {
	flags.BoolP("verbose", "v", false, "verbose output (implies --log-level=debug)")
	flags.String("log-level", "",
		"log level: debug, info, warn, error (default: info)")
	flags.Bool("log-sql", false,
		"log every SQL query at info level (verbose; for debugging)")
	flags.Int64("log-sql-slow-ms", 0,
		"threshold in ms above which a SQL query is logged as slow "+
			"(default 100; 0 uses the default)")
}

// IsLoggingFlag reports whether name is a flag AddLoggingFlags declares.
func IsLoggingFlag(name string) bool {
	return NewFlagSet().Lookup(name) != nil
}

// NewFlagSet returns a flag set for checking the arguments of a command
// run through the daemon. It already declares the forwarded logging flags;
// callers declare the command's own flags before parsing. Parse rejects
// any flag that is not declared.
func NewFlagSet() *pflag.FlagSet {
	flags := pflag.NewFlagSet("msgvault", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	AddLoggingFlags(flags)
	return flags
}
