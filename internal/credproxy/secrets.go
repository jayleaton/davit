package credproxy

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"davit/internal/domain"
)

// FileSecrets is the host SecretStore: one 0600 file per credential under a
// 0700 directory (<dataDir>/credentials by default), never in the state
// database and never in a guest. Names are hex-encoded, so no profile or
// credential name can escape the directory. Writes are atomic (temp file,
// fsync, rename), so a crash leaves the old or the new secret, never half.
//
// The files are as protected as the daemon's own data dir: anyone who can
// read them as the daemon's user can read the keys, which is also true of
// any key the daemon must be able to decrypt unattended.
type FileSecrets struct {
	Dir string
}

func (s FileSecrets) path(profileID, credential string) (dir, file string, err error) {
	if s.Dir == "" || !filepath.IsAbs(s.Dir) {
		return "", "", errors.New("credproxy: secrets dir must be absolute")
	}
	if profileID == "" || credential == "" {
		return "", "", domain.Errorf(domain.CodeInvalidArgument, "profile and credential are required")
	}
	dir = filepath.Join(s.Dir, hex.EncodeToString([]byte(profileID)))
	return dir, filepath.Join(dir, hex.EncodeToString([]byte(credential))), nil
}

// PutSecret implements SecretStore.
func (s FileSecrets) PutSecret(_ context.Context, profileID, credential string, secret []byte) error {
	dir, file, err := s.path(profileID, credential)
	if err != nil {
		return err
	}
	if len(secret) == 0 {
		return domain.Errorf(domain.CodeInvalidArgument, "secret is empty").WithDetail("field", "secret")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("credproxy: secrets dir: %w", err)
	}
	// MkdirAll keeps an existing directory's mode; insist on 0700.
	for _, d := range []string{s.Dir, dir} {
		if err := os.Chmod(d, 0o700); err != nil {
			return fmt.Errorf("credproxy: secrets dir: %w", err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("credproxy: write secret: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	err = tmp.Chmod(0o600)
	if err == nil {
		_, err = tmp.Write(secret)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), file)
	}
	if err != nil {
		return fmt.Errorf("credproxy: write secret: %w", err)
	}
	return syncDir(dir)
}

// Secret implements SecretStore.
func (s FileSecrets) Secret(_ context.Context, profileID, credential string) ([]byte, error) {
	_, file, err := s.path(profileID, credential)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, domain.Errorf(domain.CodeNotFound, "credential %s/%s has no secret", profileID, credential)
	}
	if err != nil {
		return nil, fmt.Errorf("credproxy: read secret: %w", err)
	}
	return b, nil
}

// DeleteSecret implements SecretStore; deleting a missing secret is fine.
func (s FileSecrets) DeleteSecret(_ context.Context, profileID, credential string) error {
	dir, file, err := s.path(profileID, credential)
	if err != nil {
		return err
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("credproxy: delete secret: %w", err)
	}
	_ = os.Remove(dir) // only if empty
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
