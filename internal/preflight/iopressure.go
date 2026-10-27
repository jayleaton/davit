package preflight

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// DefaultIOBusyPercent is the share of the last ten seconds in which
// some task waited for I/O (Linux PSI, /proc/pressure/io "some avg10")
// above which the host counts as busy. Interactive use of a disk keeps it
// in single digits; a build, a package install or a guest's heavy writes
// take it well past this.
const DefaultIOBusyPercent = 20

// IOPressure reads the host's I/O pressure (PSI "some avg10", percent);
// ok is false without PSI (other systems, older kernels).
func IOPressure() (float64, bool) { return ioPressure("/proc/pressure/io") }

// ioPressure reads the "some avg10" value of a PSI file.
func ioPressure(path string) (float64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "some" {
			continue
		}
		if v, ok := strings.CutPrefix(fields[1], "avg10="); ok {
			f, err := strconv.ParseFloat(v, 64)
			return f, err == nil
		}
	}
	return 0, false
}
