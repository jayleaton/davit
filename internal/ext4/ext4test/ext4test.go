// Package ext4test builds small ext2/3/4 images for tests with mke2fs -d,
// which populates a filesystem from a directory without root or a mount.
package ext4test

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Image makes an image of sizeMiB from the tree at src with mkfs (a command
// line such as "mkfs.ext4 -b 1024") and returns its path. It skips the test
// when the mkfs tool is not installed (e2fsprogs).
func Image(t testing.TB, src string, sizeMiB int, mkfs string) string {
	t.Helper()
	argv := strings.Fields(mkfs)
	if _, err := exec.LookPath(argv[0]); err != nil {
		t.Skipf("%s not installed (e2fsprogs): %v", argv[0], err)
	}
	img := filepath.Join(t.TempDir(), "fs.img")
	argv = append(argv, "-q", "-F", "-d", src, img, strconv.Itoa(sizeMiB)+"M")
	if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(argv, " "), err, out)
	}
	return img
}
