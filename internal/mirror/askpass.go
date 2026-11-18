package mirror

import (
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	askpassFileEnv = "WARD_ASKPASS_FILE"
	askpassUserEnv = "WARD_ASKPASS_USER"
)

// MaybeAskpass runs the askpass helper when the environment asks for it. It returns false for a normal start.
// git runs the program of GIT_ASKPASS with the prompt as its only argument, and the program is the ward binary.
func MaybeAskpass(args []string, stdout io.Writer) bool {
	file := os.Getenv(askpassFileEnv)
	if file == "" {
		return false
	}
	if len(args) > 0 && strings.HasPrefix(strings.ToLower(args[0]), "username") {
		fmt.Fprintln(stdout, os.Getenv(askpassUserEnv))
		return true
	}
	secret, err := os.ReadFile(file)
	if err != nil {
		os.Exit(1)
	}
	fmt.Fprintln(stdout, strings.TrimSpace(string(secret)))
	return true
}
