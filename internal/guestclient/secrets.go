package guestclient

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"davit/internal/runtime"
)

// SecretStore keeps the per-generation agent secrets the host provisioned.
// Load returns the candidates (newest first) a guest proof may verify
// under; Save replaces them, and an empty list forgets the key. Saving a
// key drops secrets of every other generation of the same VM.
//
// The prober saves {new, old} before sending SetIdentity.NewAgentSecret (on
// a bootstrap, in the same write as the bootstrap secret), so a crash at any
// point never strands a guest whose secret the host forgot. The old
// candidate is left in place after the rotation: it no longer verifies
// (the guest dropped it) and pruning it would cost a durable write on the
// restore barrier's critical path.
type SecretStore interface {
	Load(ctx context.Context, key runtime.Key) ([][]byte, error)
	Save(ctx context.Context, key runtime.Key, secrets [][]byte) error
}

// MemorySecrets is an in-process SecretStore. Secrets are lost on daemon
// restart, after which the host can no longer authenticate running guests
// of that generation; use DirSecrets in the daemon.
type MemorySecrets struct {
	mu sync.Mutex
	m  map[runtime.Key][][]byte
}

func (s *MemorySecrets) Load(_ context.Context, key runtime.Key) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSecrets(s.m[key]), nil
}

func (s *MemorySecrets) Save(_ context.Context, key runtime.Key, secrets [][]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[runtime.Key][][]byte{}
	}
	for k := range s.m {
		if k.VMID == key.VMID {
			delete(s.m, k)
		}
	}
	if len(secrets) > 0 {
		s.m[key] = cloneSecrets(secrets)
	}
	return nil
}

func cloneSecrets(in [][]byte) [][]byte {
	out := make([][]byte, len(in))
	for i, s := range in {
		out[i] = bytes.Clone(s)
	}
	return out
}

// DirSecrets stores secrets as owner-only files <Dir>/<vmId>.<generation>
// (one hex secret per line), written atomically and fsynced. Dir must be
// private to the daemon; it is created 0700.
type DirSecrets struct{ Dir string }

func (s DirSecrets) path(key runtime.Key) (string, error) {
	id := string(key.VMID)
	if id == "" || strings.ContainsAny(id, "/\\.*?[\x00") || key.Generation <= 0 {
		return "", fmt.Errorf("guestclient: invalid secret key %q/%d", id, key.Generation)
	}
	return filepath.Join(s.Dir, id+"."+strconv.FormatInt(key.Generation, 10)), nil
}

func (s DirSecrets) Load(_ context.Context, key runtime.Key) ([][]byte, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for _, line := range strings.Fields(string(raw)) {
		b, err := hex.DecodeString(line)
		if err != nil {
			return nil, fmt.Errorf("guestclient: corrupt secret file for %s", key.VMID)
		}
		out = append(out, b)
	}
	return out, nil
}

func (s DirSecrets) Save(_ context.Context, key runtime.Key, secrets [][]byte) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if len(secrets) > 0 {
		var b strings.Builder
		for _, sec := range secrets {
			b.WriteString(hex.EncodeToString(sec))
			b.WriteByte('\n')
		}
		if err := writeFileSync(p, []byte(b.String())); err != nil {
			return err
		}
	}
	// Drop other generations (and this one when forgetting).
	matches, err := filepath.Glob(filepath.Join(s.Dir, string(key.VMID)+".*"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		if m != p || len(secrets) == 0 {
			if err := os.Remove(m); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func writeFileSync(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
