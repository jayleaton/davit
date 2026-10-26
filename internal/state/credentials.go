package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"davit/internal/domain"
)

// Credential lookups (G4). Profiles and grants are metadata only: secrets
// live in the host secret store, never here.

// GetCredentialProfile returns owner's profile by ID or name.
func (t *Tx) GetCredentialProfile(owner domain.PrincipalID, ref string) (*domain.CredentialProfile, error) {
	var doc string
	err := t.row(`SELECT doc FROM credential_profiles WHERE owner = ? AND (id = ? OR name = ?)`, owner, ref, ref).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.Errorf(domain.CodeNotFound, "credential profile %s not found", ref)
	}
	if err != nil {
		return nil, err
	}
	var p domain.CredentialProfile
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return nil, err
	}
	p.Owner = owner
	return &p, nil
}

// ListCredentialProfiles pages owner's profiles in (createdAt, id) order.
func (t *Tx) ListCredentialProfiles(owner domain.PrincipalID, p Page) ([]*domain.CredentialProfile, string, error) {
	c, err := p.decode()
	if err != nil {
		return nil, "", err
	}
	rows, err := t.query(`SELECT doc, created_ns, id FROM credential_profiles
WHERE owner = ? AND (created_ns, id) > (?, ?) ORDER BY created_ns, id LIMIT ?`, owner, c.CreatedNS, c.ID, p.Limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []*domain.CredentialProfile{}
	var lastNS int64
	var lastID string
	for rows.Next() {
		var doc, id string
		var ns int64
		if err := rows.Scan(&doc, &ns, &id); err != nil {
			return nil, "", err
		}
		if len(out) == p.Limit {
			return out, encodeCursor(lastNS, lastID, p.Filter), rows.Err()
		}
		var cp domain.CredentialProfile
		if err := json.Unmarshal([]byte(doc), &cp); err != nil {
			return nil, "", err
		}
		cp.Owner = owner
		out = append(out, &cp)
		lastNS, lastID = ns, id
	}
	return out, "", rows.Err()
}

// DeleteCredentialProfile removes a profile row (its grants are revoked by
// the caller first).
func (t *Tx) DeleteCredentialProfile(id string) error {
	_, err := t.exec(`DELETE FROM credential_profiles WHERE id = ?`, id)
	return err
}

// GetGrant returns a grant by ID with its owner.
func (t *Tx) GetGrant(id string) (*domain.CredentialGrant, domain.PrincipalID, error) {
	return t.scanGrant(t.row(`SELECT doc, owner FROM credential_grants WHERE id = ?`, id), "grant "+id)
}

// GrantByPlaceholder returns the grant holding placeholder, with its owner.
func (t *Tx) GrantByPlaceholder(placeholder string) (*domain.CredentialGrant, domain.PrincipalID, error) {
	return t.scanGrant(t.row(`SELECT doc, owner FROM credential_grants WHERE placeholder = ?`, placeholder), "placeholder")
}

func (t *Tx) scanGrant(r *sql.Row, what string) (*domain.CredentialGrant, domain.PrincipalID, error) {
	var doc, owner string
	err := r.Scan(&doc, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", domain.Errorf(domain.CodeNotFound, "%s not found", what)
	}
	if err != nil {
		return nil, "", err
	}
	var g domain.CredentialGrant
	return &g, domain.PrincipalID(owner), json.Unmarshal([]byte(doc), &g)
}

// ActiveProfileGrants returns the active grants of a profile, with owners.
func (t *Tx) ActiveProfileGrants(profileID string) ([]*domain.CredentialGrant, []domain.PrincipalID, error) {
	rows, err := t.query(`SELECT doc, owner FROM credential_grants WHERE profile_id = ? AND state = 'active' ORDER BY created_ns, id`, profileID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var gs []*domain.CredentialGrant
	var owners []domain.PrincipalID
	for rows.Next() {
		var doc, owner string
		if err := rows.Scan(&doc, &owner); err != nil {
			return nil, nil, err
		}
		var g domain.CredentialGrant
		if err := json.Unmarshal([]byte(doc), &g); err != nil {
			return nil, nil, err
		}
		gs, owners = append(gs, &g), append(owners, domain.PrincipalID(owner))
	}
	return gs, owners, rows.Err()
}

// Creates with an Idempotency-Key (createCredentialProfile,
// createCredentialGrant) keep the key and the request's body hash in the
// row's doc next to the record, so a replay returns the same record. The
// tables have no key column; readers that decode only the domain type
// ignore these fields.
type idemFields struct {
	IdemKey  string `json:"_idempotencyKey,omitempty"`
	IdemHash string `json:"_bodyHash,omitempty"`
}

// PutCredentialProfileKeyed is PutCredentialProfile that also records (or,
// on update, keeps) the create's idempotency key and body hash.
func (t *Tx) PutCredentialProfileKeyed(p *domain.CredentialProfile, key, hash string) error {
	doc, err := json.Marshal(struct {
		*domain.CredentialProfile
		idemFields
	}{p, idemFields{key, hash}})
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO credential_profiles (id, owner, name, created_ns, doc) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, doc=excluded.doc`, p.ID, p.Owner, p.Name, p.CreatedAt.UnixNano(), string(doc))
	if err != nil && isUnique(err) {
		return domain.Errorf(domain.CodeInvalidArgument, "credential profile name %q is taken", p.Name).WithDetail("field", "name")
	}
	return err
}

// CredentialProfileKeys returns the idempotency key and body hash recorded
// with owner's profile id.
func (t *Tx) CredentialProfileKeys(owner domain.PrincipalID, id string) (key, hash string, err error) {
	var doc string
	if err := t.row(`SELECT doc FROM credential_profiles WHERE owner = ? AND id = ?`, owner, id).Scan(&doc); err != nil {
		return "", "", err
	}
	var f idemFields
	return f.IdemKey, f.IdemHash, json.Unmarshal([]byte(doc), &f)
}

// CredentialProfileByKey returns owner's profile created with key, and the
// body hash of that create.
func (t *Tx) CredentialProfileByKey(owner domain.PrincipalID, key string) (*domain.CredentialProfile, string, error) {
	var doc string
	err := t.row(`SELECT doc FROM credential_profiles WHERE owner = ? AND json_extract(doc, '$._idempotencyKey') = ?`, owner, key).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", domain.Errorf(domain.CodeNotFound, "no profile for this key")
	}
	if err != nil {
		return nil, "", err
	}
	var p domain.CredentialProfile
	var f idemFields
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return nil, "", err
	}
	p.Owner = owner
	return &p, f.IdemHash, json.Unmarshal([]byte(doc), &f)
}

// PutGrantKeyed inserts a new grant with the create's idempotency key and
// body hash.
func (t *Tx) PutGrantKeyed(owner domain.PrincipalID, g *domain.CredentialGrant, key, hash string) error {
	doc, err := json.Marshal(struct {
		*domain.CredentialGrant
		idemFields
	}{g, idemFields{key, hash}})
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO credential_grants (id, vm_id, owner, profile_id, state, placeholder, created_ns, doc)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, g.ID, g.VMID, owner, g.ProfileID, g.State, g.Placeholder, g.CreatedAt.UnixNano(), string(doc))
	return err
}

// GrantByKey returns vm's grant created with key, and that create's body
// hash.
func (t *Tx) GrantByKey(owner domain.PrincipalID, vm domain.VMID, key string) (*domain.CredentialGrant, string, error) {
	var doc string
	err := t.row(`SELECT doc FROM credential_grants WHERE owner = ? AND vm_id = ? AND json_extract(doc, '$._idempotencyKey') = ?`, owner, vm, key).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", domain.Errorf(domain.CodeNotFound, "no grant for this key")
	}
	if err != nil {
		return nil, "", err
	}
	var g domain.CredentialGrant
	var f idemFields
	if err := json.Unmarshal([]byte(doc), &g); err != nil {
		return nil, "", err
	}
	return &g, f.IdemHash, json.Unmarshal([]byte(doc), &f)
}

// RevokeGrantsWhere revokes the active grants matching one of the filters
// (exactly one non-empty: grant ID, VM, profile, or profile+credential),
// keeping every other doc field, and returns the IDs it revoked. A revoked
// grant keeps its first reason.
func (t *Tx) RevokeGrantsWhere(f GrantFilter, reason domain.RevokeReason, now time.Time) ([]string, error) {
	var where string
	var args []any
	switch {
	case f.ID != "":
		where, args = "id = ?", []any{f.ID}
	case f.VM != "":
		where, args = "vm_id = ?", []any{f.VM}
	case f.Profile != "" && f.Credential != "":
		where, args = "profile_id = ? AND json_extract(doc, '$.credential') = ?", []any{f.Profile, f.Credential}
	case f.Profile != "":
		where, args = "profile_id = ?", []any{f.Profile}
	default:
		return nil, errors.New("state: empty grant filter")
	}
	rows, err := t.query(`SELECT id FROM credential_grants WHERE state = 'active' AND `+where, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	at, _ := json.Marshal(now.UTC())
	for _, id := range ids {
		if _, err := t.exec(`UPDATE credential_grants SET state = 'revoked',
doc = json_set(doc, '$.state', 'revoked', '$.revokeReason', ?, '$.revokedAt', json(?)) WHERE id = ? AND state = 'active'`,
			string(reason), string(at), id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// GrantFilter selects grants for RevokeGrantsWhere.
type GrantFilter struct {
	ID         string
	VM         domain.VMID
	Profile    string
	Credential string
}
