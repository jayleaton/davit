package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"

	"davit/internal/harness"
)

// secretsFile reads the harness secrets file on every use. It must be a
// regular file owned by this user with no group/other access.
func secretsFile(path string) harness.Secrets {
	if path == "" {
		return nil
	}
	return func(name string) (string, bool, error) {
		fi, err := os.Stat(path)
		if err != nil {
			return "", false, err
		}
		if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
			return "", false, fmt.Errorf("%s must be a regular file with mode 0600", path)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
			return "", false, fmt.Errorf("%s must be owned by the daemon user", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", false, err
		}
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", false, fmt.Errorf("%s: want a JSON object of strings: %v", path, err)
		}
		v, ok := m[name]
		return v, ok, nil
	}
}
