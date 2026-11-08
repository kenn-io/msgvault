package clirun

import (
	"fmt"
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
// callers declare the command's own flags and then call Parse, which
// rejects any flag that is not declared.
func NewFlagSet() *pflag.FlagSet {
	flags := pflag.NewFlagSet("msgvault", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	AddLoggingFlags(flags)
	return flags
}

// Parse parses args into flags and rejects a flag given more than once. A
// check validates one value, but the subprocess's Cobra keeps the last and
// the original arguments still reach it, so a repeated flag could carry a
// value the check never saw.
func Parse(flags *pflag.FlagSet, args []string) error {
	seen := make(map[string]bool)
	return flags.ParseAll(args, func(flag *pflag.Flag, value string) error {
		if seen[flag.Name] {
			return fmt.Errorf("flag --%s is given more than once", flag.Name)
		}
		seen[flag.Name] = true
		return flags.Set(flag.Name, value)
	})
}
