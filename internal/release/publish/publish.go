// Package publish is the seam of the release destination. Implementations:
// a directory (publish/dir) and GitHub releases through gh (publish/github).
// The conformance suite is publish/publishtest.
package publish

// Publisher sends release artifacts to a destination.
type Publisher interface {
	// Publish makes the files visible under their base names for tag. A
	// later Publish of the same names replaces them. It fails when a file
	// cannot be read.
	Publish(tag string, files []string) error
}
