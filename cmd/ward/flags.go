package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// parseFlags parses args. A request for help prints usage to stdout and returns code 0 with done set; any other flag error
// prints the error and usage to stderr and returns code 2 with done set.
func parseFlags(fs *flag.FlagSet, args []string, usage string, stdout, stderr io.Writer) (code int, done bool) {
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	if err == nil {
		return 0, false
	}
	out, code := stderr, 2
	if errors.Is(err, flag.ErrHelp) {
		out, code = stdout, 0
	} else {
		fmt.Fprintln(out, err)
	}
	fmt.Fprintln(out, "usage: "+usage)
	fs.SetOutput(out)
	fs.PrintDefaults()
	return code, true
}

func isHelp(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "-help" || arg == "help"
}

// wantsHelp reports whether args ask for the help of the command, of version, or of runner. The other subcommands parse
// their own help flags.
func wantsHelp(args []string) bool {
	switch {
	case len(args) == 0:
		return false
	case isHelp(args[0]):
		return true
	case len(args) == 2 && (args[0] == "version" || args[0] == "runner"):
		return isHelp(args[1])
	}
	return false
}
