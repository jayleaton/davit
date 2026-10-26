package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// OwnerMatcher decides whether a tailnet identity is the host owner.
// OwnerPolicy matches configuration alone; BoundOwners pins each configured
// login to the stable tailnet user ID it first matched.
type OwnerMatcher interface {
	IsOwner(id Identity) bool
}

// BoundOwners matches untagged owners by stable tailnet user ID. The
// configured logins are only the bootstrap: the first identity whose login
// matches binds that login to its user ID (persisted), and from then on the
// owner is whoever holds a bound user ID, whatever its login is now. A login
// that is renamed keeps working; a login that is recycled or reassigned to
// another user at the coordination server (or a Headscale user renamed onto
// it) does not. Removing a login from the configuration retires its
// binding. Tagged nodes match by ACL tag, which the tailnet policy asserts.
type BoundOwners struct {
	policy OwnerPolicy
	path   string
	logf   func(string, ...any)

	mu    sync.Mutex
	bound map[string]OwnerBinding // lowercase login -> binding
}

// OwnerBinding is one configured login pinned to a stable user ID.
type OwnerBinding struct {
	Login  string `json:"login"`
	UserID int64  `json:"userId"`
	// NodeID is the node the binding was made from (informational).
	NodeID  string    `json:"nodeId"`
	BoundAt time.Time `json:"boundAt"`
}

type ownersFile struct {
	Version  int            `json:"version"`
	Bindings []OwnerBinding `json:"bindings"`
}

// OpenOwners loads the bindings at path (created on first bind, 0600).
func OpenOwners(path string, p OwnerPolicy, logf func(string, ...any)) (*BoundOwners, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	o := &BoundOwners{policy: p, path: path, logf: logf, bound: map[string]OwnerBinding{}}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return o, nil
	case err != nil:
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("gateway: %s must not be group/world accessible (mode %v)", path, fi.Mode().Perm())
	}
	var f ownersFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("gateway: %s: %w", path, err)
	}
	for _, bd := range f.Bindings {
		if bd.UserID == 0 || bd.Login == "" {
			return nil, fmt.Errorf("gateway: %s: invalid binding %+v", path, bd)
		}
		o.bound[strings.ToLower(bd.Login)] = bd
	}
	return o, nil
}

// Bindings returns the active bindings (configured logins only), sorted.
func (o *BoundOwners) Bindings() []OwnerBinding {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []OwnerBinding
	for login, b := range o.bound {
		if o.configured(login) {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(a, b OwnerBinding) int { return strings.Compare(a.Login, b.Login) })
	return out
}

func (o *BoundOwners) configured(login string) bool {
	return slices.ContainsFunc(o.policy.Logins, func(l string) bool { return strings.EqualFold(l, login) })
}

// IsOwner implements OwnerMatcher.
func (o *BoundOwners) IsOwner(id Identity) bool {
	if id.Tagged() {
		return o.policy.IsOwner(id)
	}
	if id.UserID == 0 {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for login, b := range o.bound {
		if b.UserID == id.UserID && o.configured(login) {
			return true
		}
	}
	login := strings.ToLower(id.LoginName)
	if login == "" || !o.configured(login) {
		return false
	}
	if b, ok := o.bound[login]; ok {
		o.logf("gateway: refusing %s: login %s is bound to tailnet user %d, not %d", id, b.Login, b.UserID, id.UserID)
		return false
	}
	b := OwnerBinding{Login: login, UserID: id.UserID, NodeID: id.NodeID, BoundAt: time.Now().UTC()}
	o.bound[login] = b
	if err := o.save(); err != nil {
		delete(o.bound, login)
		o.logf("gateway: refusing %s: cannot record owner binding: %v", id, err)
		return false // never grant a match we could not pin
	}
	o.logf("gateway: owner login %s bound to tailnet user %d (node %s)", login, id.UserID, id.NodeID)
	return true
}

// save writes every binding (including retired ones, so re-adding a login
// keeps its pin) atomically. Callers hold o.mu.
func (o *BoundOwners) save() error {
	f := ownersFile{Version: 1}
	for _, b := range o.bound {
		f.Bindings = append(f.Bindings, b)
	}
	slices.SortFunc(f.Bindings, func(a, b OwnerBinding) int { return strings.Compare(a.Login, b.Login) })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(o.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(o.path), ".owners-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	err = tmp.Chmod(0o600)
	if err == nil {
		_, err = tmp.Write(append(data, '\n'))
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), o.path)
}
