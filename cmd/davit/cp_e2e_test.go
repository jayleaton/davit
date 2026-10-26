//go:build linux

package main_test

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"davit/api"
	"davit/internal/cli"
	"davit/internal/domain"
)

// TestCpEndToEnd copies files and directories through the in-process guest
// agent of the fake runtime. Its "guest" filesystem is this host, so guest
// paths are temp dirs.
func TestCpEndToEnd(t *testing.T) {
	h := newHost(t)
	h.guestAgent = true
	h.start(20*time.Millisecond, 0)
	h.waited(h.cli("new", "box"), domain.OpCreate)
	local, guest := t.TempDir(), t.TempDir()

	// One file each way, mode preserved, through a VM directory target.
	data := make([]byte, 3<<20+5)
	rand.Read(data)
	src := filepath.Join(local, "blob.bin")
	os.WriteFile(src, data, 0o640)
	r := h.cli("cp", src, "box:"+guest+"/")
	if r.code != 0 || r.last().Event != "result" {
		t.Fatalf("upload: exit %d %+v", r.code, r.last())
	}
	var res struct {
		Bytes int64
		File  api.FileInfo
	}
	json.Unmarshal(r.raw[len(r.raw)-1], &res)
	if res.Bytes != int64(len(data)) || res.File.Mode != "0640" || len(res.File.SHA256) != 64 {
		t.Fatalf("upload result %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(guest, "blob.bin")); !bytes.Equal(b, data) {
		t.Fatal("uploaded bytes differ")
	}
	back := filepath.Join(local, "back.bin")
	if r := h.cli("cp", "box:"+guest+"/blob.bin", back); r.code != 0 {
		t.Fatalf("download: exit %d %+v", r.code, r.last())
	}
	if b, _ := os.ReadFile(back); !bytes.Equal(b, data) {
		t.Fatal("downloaded bytes differ")
	}
	if fi, _ := os.Stat(back); fi.Mode().Perm() != 0o640 {
		t.Fatalf("downloaded mode %v", fi.Mode())
	}

	// A directory tree round trip with -r keeps modes and symlinks.
	tree := filepath.Join(local, "tree")
	os.MkdirAll(filepath.Join(tree, "bin"), 0o755)
	os.WriteFile(filepath.Join(tree, "bin", "tool"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(tree, "notes.txt"), []byte("hi"), 0o600)
	os.Symlink("bin/tool", filepath.Join(tree, "tool"))
	os.Chmod(tree, 0o750)
	if r := h.cli("cp", "-r", tree, "box:"+guest+"/"); r.code != 0 {
		t.Fatalf("upload -r: exit %d %+v", r.code, r.last())
	}
	if fi, err := os.Stat(filepath.Join(guest, "tree", "bin", "tool")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("uploaded tree: %v %v", fi, err)
	}
	if fi, _ := os.Stat(filepath.Join(guest, "tree")); fi.Mode().Perm() != 0o750 {
		t.Fatalf("uploaded root mode %v", fi.Mode())
	}
	out := filepath.Join(local, "out")
	if r := h.cli("cp", "-r", "box:"+guest+"/tree", out); r.code != 0 {
		t.Fatalf("download -r: exit %d %+v", r.code, r.last())
	}
	if l, err := os.Readlink(filepath.Join(out, "tool")); err != nil || l != "bin/tool" {
		t.Fatalf("symlink %q %v", l, err)
	}
	if fi, _ := os.Stat(filepath.Join(out, "notes.txt")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("notes mode %v", fi.Mode())
	}

	h.expectError(h.cli("cp", tree, "box:"+guest+"/x"), cli.ExitUsage, domain.CodeInvalidArgument)        // dir without -r
	h.expectError(h.cli("cp", src, "box:relative"), cli.ExitUsage, domain.CodeInvalidArgument)            // relative VM path
	h.expectError(h.cli("cp", src, filepath.Join(local, "x")), cli.ExitUsage, domain.CodeInvalidArgument) // no VM side
	h.expectError(h.cli("cp", "box:"+guest+"/missing", local), cli.ExitNotFound, domain.CodeNotFound)
}
