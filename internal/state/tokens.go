package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"davit/api"
	"davit/internal/domain"
)

// TokenRecord is one service token. Only Hash (SHA-256 of the secret) is
// stored, never the secret.
type TokenRecord struct {
	Token          api.Token          `json:"token"`
	Owner          domain.PrincipalID `json:"owner"`
	Hash           []byte             `json:"-"`
	IdempotencyKey string             `json:"-"`
	BodyHash       string             `json:"-"`
	RevokedAt      *time.Time         `json:"revokedAt,omitempty"`
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

// PutToken inserts or updates a token (revocation, last use).
func (t *Tx) PutToken(r *TokenRecord) error {
	doc, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO tokens (id, owner, hash, idempotency_key, body_hash, created_ns, expires_ns, revoked_ns, doc)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET expires_ns=excluded.expires_ns, revoked_ns=excluded.revoked_ns, doc=excluded.doc`,
		r.Token.ID, r.Owner, r.Hash, nullString(r.IdempotencyKey), r.BodyHash, r.Token.CreatedAt.UnixNano(),
		r.Token.ExpiresAt.UnixNano(), nullTime(r.RevokedAt), string(doc))
	return err
}

func (t *Tx) scanToken(row *sql.Row) (*TokenRecord, error) {
	var doc string
	var hash []byte
	var key sql.NullString
	var body string
	if err := row.Scan(&doc, &hash, &key, &body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.Errorf(domain.CodeNotFound, "token not found")
		}
		return nil, err
	}
	var r TokenRecord
	if err := json.Unmarshal([]byte(doc), &r); err != nil {
		return nil, err
	}
	r.Hash, r.IdempotencyKey, r.BodyHash = hash, key.String, body
	return &r, nil
}

const tokenCols = `doc, hash, idempotency_key, body_hash`

// GetToken returns owner's token by ID (revoked ones included).
func (t *Tx) GetToken(owner domain.PrincipalID, id string) (*TokenRecord, error) {
	return t.scanToken(t.row(`SELECT `+tokenCols+` FROM tokens WHERE id = ? AND owner = ?`, id, owner))
}

// TokenByHash returns the token whose secret hashes to hash.
func (t *Tx) TokenByHash(hash []byte) (*TokenRecord, error) {
	return t.scanToken(t.row(`SELECT `+tokenCols+` FROM tokens WHERE hash = ?`, hash))
}

// TokenByKey returns owner's token created with idempotency key.
func (t *Tx) TokenByKey(owner domain.PrincipalID, key string) (*TokenRecord, error) {
	return t.scanToken(t.row(`SELECT `+tokenCols+` FROM tokens WHERE owner = ? AND idempotency_key = ?`, owner, key))
}

// ListTokens pages owner's tokens that are neither revoked nor expired at
// now, ordered by (createdAt, id).
func (t *Tx) ListTokens(owner domain.PrincipalID, now time.Time, p Page) ([]*TokenRecord, string, error) {
	c, err := p.decode()
	if err != nil {
		return nil, "", err
	}
	rows, err := t.query(`SELECT `+tokenCols+`, created_ns, id FROM tokens
WHERE owner = ? AND revoked_ns IS NULL AND expires_ns > ? AND (created_ns, id) > (?, ?)
ORDER BY created_ns, id LIMIT ?`, owner, now.UnixNano(), c.CreatedNS, c.ID, p.Limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []*TokenRecord{}
	var lastNS int64
	var lastID string
	for rows.Next() {
		var doc, body, id string
		var hash []byte
		var key sql.NullString
		var ns int64
		if err := rows.Scan(&doc, &hash, &key, &body, &ns, &id); err != nil {
			return nil, "", err
		}
		if len(out) == p.Limit {
			return out, encodeCursor(lastNS, lastID, p.Filter), rows.Err()
		}
		var r TokenRecord
		if err := json.Unmarshal([]byte(doc), &r); err != nil {
			return nil, "", err
		}
		r.Hash, r.IdempotencyKey, r.BodyHash = hash, key.String, body
		out = append(out, &r)
		lastNS, lastID = ns, id
	}
	return out, "", rows.Err()
}
