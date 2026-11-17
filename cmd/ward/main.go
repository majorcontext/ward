// Command ward is the forge service on top of motte.
package main

import "fmt"

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() { fmt.Println("ward", version) }
