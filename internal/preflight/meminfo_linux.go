//go:build linux

package preflight

import "os"

// FreeMemory is the host memory available for new work without swapping
// (MemAvailable in /proc/meminfo), in bytes.
func FreeMemory() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return parseMeminfoField(f, "MemAvailable:")
}

// TotalMemory is the host's memory (MemTotal in /proc/meminfo), in bytes.
func TotalMemory() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return parseMeminfoField(f, "MemTotal:")
}
