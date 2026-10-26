package preflight

import (
	"strings"
	"testing"
)

func TestParseMemAvailable(t *testing.T) {
	in := "MemTotal:       65536000 kB\nMemFree:         1000 kB\nMemAvailable:   40000000 kB\nBuffers: 1 kB\n"
	if n, err := parseMeminfoField(strings.NewReader(in), "MemAvailable:"); err != nil || n != 40000000*1024 {
		t.Fatalf("got %d, %v", n, err)
	}
	if _, err := parseMeminfoField(strings.NewReader("MemTotal: 1 kB\n"), "MemAvailable:"); err == nil {
		t.Fatal("missing MemAvailable accepted")
	}
	if _, err := parseMeminfoField(strings.NewReader("MemAvailable: x kB\n"), "MemAvailable:"); err == nil {
		t.Fatal("garbage accepted")
	}
}
