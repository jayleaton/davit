package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStorageTiersDocConfigsLoad loads every ```json block of
// docs/storage-tiers.md through Load: each must be a complete host config
// that the daemon accepts (fragments use another fence language).
func TestStorageTiersDocConfigsLoad(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "storage-tiers.md"))
	if err != nil {
		t.Fatal(err)
	}
	var heading, fence string
	var block []string
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case fence != "" && trimmed == "```":
			if fence == "json" {
				n++
				body := strings.Join(block, "\n")
				t.Run(fmt.Sprintf("%d %s", n, heading), func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "config.json")
					if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
						t.Fatal(err)
					}
					if _, err := Load(path); err != nil {
						t.Errorf("%v\n%s", err, body)
					}
				})
			}
			fence, block = "", nil
		case fence != "":
			block = append(block, line)
		case strings.HasPrefix(trimmed, "```"):
			fence = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			if fence == "" {
				fence = "text"
			}
		case strings.HasPrefix(line, "#"):
			heading = strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
	}
	if n == 0 {
		t.Fatal("docs/storage-tiers.md has no ```json blocks")
	}
}
