//go:build !linux

package netnsdns

import (
	"fmt"
	"io"
)

// Main reports that the resolver needs Linux network namespaces.
func Main(_ []string, stderr io.Writer) int {
	fmt.Fprintln(stderr, "netns-dns runs only on Linux")
	return 1
}
