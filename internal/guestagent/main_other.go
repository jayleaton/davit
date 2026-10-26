//go:build !linux

package guestagent

import (
	"fmt"
	"io"

	"davit/internal/buildinfo"
)

// Main reports that the guest agent runs only inside Linux guests.
func Main(_ []string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "%s guest-agent: runs only inside Linux guests\n", buildinfo.Name)
	return 1
}
