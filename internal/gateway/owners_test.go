package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOwnersBindLoginToStableUserID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "owners.json")
	policy := OwnerPolicy{Logins: []string{"Alice@example.com"}, Tags: []string{"tag:ci"}}
	o, err := OpenOwners(path, policy, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	alice := Identity{UserID: 7, LoginName: "alice@example.com", NodeID: "n1"}
	impostor := Identity{UserID: 8, LoginName: "alice@example.com", NodeID: "n9"} // login recycled or reassigned
	if !o.IsOwner(alice) {
		t.Fatal("configured login did not bootstrap")
	}
	if o.IsOwner(impostor) {
		t.Fatal("a different user with the bound login was accepted")
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("owners file: %v %v", fi, err)
	}

	// The pin survives a restart; a renamed login still matches by ID.
	o, err = OpenOwners(path, policy, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if !o.IsOwner(alice) || o.IsOwner(impostor) || !o.IsOwner(Identity{UserID: 7, LoginName: "alice.renamed@example.com", NodeID: "n2"}) {
		t.Fatal("binding lost or not matched by ID after reopen")
	}
	if b := o.Bindings(); len(b) != 1 || b[0].UserID != 7 || b[0].NodeID != "n1" {
		t.Fatalf("bindings %+v", b)
	}
	// Tags are ACL assertions and still match by tag.
	if !o.IsOwner(Identity{UserID: 99, LoginName: "tagged-devices", NodeID: "n3", Tags: []string{"tag:ci"}}) {
		t.Fatal("owner tag no longer matches")
	}

	// Removing the login from the configuration retires the binding; adding it
	// back restores the same pin rather than re-bootstrapping.
	o, _ = OpenOwners(path, OwnerPolicy{Logins: []string{"bob@example.com"}}, t.Logf)
	if o.IsOwner(alice) || len(o.Bindings()) != 0 {
		t.Fatal("retired login still matches")
	}
	o, _ = OpenOwners(path, policy, t.Logf)
	if !o.IsOwner(alice) || o.IsOwner(impostor) {
		t.Fatal("re-added login lost its pin")
	}

	// Fail closed when the binding cannot be recorded.
	ro := filepath.Join(t.TempDir(), "ro")
	os.Mkdir(ro, 0o500)
	defer os.Chmod(ro, 0o700)
	o, _ = OpenOwners(filepath.Join(ro, "owners.json"), policy, t.Logf)
	if os.Getuid() != 0 && o.IsOwner(alice) {
		t.Fatal("matched without being able to persist the binding")
	}
	if o.IsOwner(Identity{LoginName: "alice@example.com", NodeID: "n1"}) {
		t.Fatal("an identity without a user ID matched")
	}
}
