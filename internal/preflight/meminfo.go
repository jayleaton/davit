package preflight

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"
)

// parseMeminfoField returns field (e.g. "MemAvailable:") in bytes from
// /proc/meminfo text.
func parseMeminfoField(r io.Reader, field string) (int64, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), field)
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			break
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			return 0, err
		}
		if len(f) > 1 && f[1] == "kB" {
			n *= 1024
		}
		return n, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("meminfo has no " + strings.TrimSuffix(field, ":"))
}
