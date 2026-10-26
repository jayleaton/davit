//go:build !linux

package netnslisten

import (
	"fmt"
	"io"
)

// Main reports that netns listeners need Linux network namespaces.
func Main(_ []string, stderr io.Writer) int {
	fmt.Fprintln(stderr, "netns-listenfd runs only on Linux")
	return 1
}
