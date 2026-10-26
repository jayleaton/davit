//go:build !linux

package userspace

import (
	"fmt"
	"io"
)

// Main reports that userspace egress needs Linux network namespaces.
func Main(_ []string, stderr io.Writer) int {
	fmt.Fprintln(stderr, "userspace egress runs only on Linux")
	return 1
}
