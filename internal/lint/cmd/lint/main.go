// Command lint runs the repository analyzers. Use: go run ./internal/lint/cmd/lint ./...
package main

import (
	"golang.org/x/tools/go/analysis/singlechecker"

	"github.com/majorcontext/ward/internal/lint/wallclock"
)

func main() { singlechecker.Main(wallclock.Analyzer) }
