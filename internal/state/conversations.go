package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"davit/internal/domain"
)

// ConversationRecord is a harness conversation on one VM: a sequence of
// turns that share a harness-native session.
type ConversationRecord struct {
	ID      string             `json:"id"`
	VMID    domain.VMID        `json:"vmId"`
	Owner   domain.PrincipalID `json:"owner"`
	Harness string             `json:"harness"`
	Model   string             `json:"model,omitempty"`
	Cwd     string             `json:"cwd,omitempty"`
	// SessionID is the harness-native session that --resume continues.
	SessionID string `json:"sessionId,omitempty"`
	// ActivePromptID is the turn in progress; one at a time.
	ActivePromptID string    `json:"activePromptId,omitempty"`
	Turns          int       `json:"turns"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// PromptRecord is one turn.
type PromptRecord struct {
	ID             string             `json:"id"`
	ConversationID string             `json:"conversationId"`
	VMID           domain.VMID        `json:"vmId"`
	Owner          domain.PrincipalID `json:"owner"`
	Harness        string             `json:"harness"`
	Model          string             `json:"model,omitempty"`
	Text           string             `json:"text"`
	// EnvNames and SecretNames name the variables the turn received; values
	// are never stored.
	EnvNames    []string        `json:"envNames,omitempty"`
	SecretNames []string        `json:"secretNames,omitempty"`
	State       string          `json:"state"` // starting | running | succeeded | failed | cancelled
	ProcessID   string          `json:"processId,omitempty"`
	Generation  int64           `json:"generation"`
	Error       json.RawMessage `json:"error,omitempty"`
	Usage       json.RawMessage `json:"usage,omitempty"`
	Events      int64           `json:"events"` // last event seq
	CreatedAt   time.Time       `json:"createdAt"`
	FinishedAt  *time.Time      `json:"finishedAt,omitempty"`
	// TurnDeadline ends the active-turn lease: the harness is killed then,
	// and until then the turn holds the VM's auto-stop.
	TurnDeadline *time.Time `json:"turnDeadlineAt,omitempty"`
	// ReasoningEffort and Fast are the turn's PromptOptions (a restart
	// steer carries them to the next turn).
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	Fast            bool   `json:"fast,omitempty"`
	// GrantEnv names the variables set from the VM's credential grants
	// (placeholders and proxy base URLs; values are never stored). Notice,
	// when set, is the turn's first event: grants replaced variables the
	// caller set.
	GrantEnv []string        `json:"grantEnv,omitempty"`
	Notice   json.RawMessage `json:"notice,omitempty"`
	// Steers are the steers delivered with an Idempotency-Key, by key.
	Steers map[string]SteerRecord `json:"steers,omitempty"`
	// Messages counts the user messages written to a streaming harness
	// (the prompt plus native steers); the turn ends after as many results.
	Messages int `json:"messages,omitempty"`
	// IdempotencyKey and BodyHash dedupe createPrompt (owner, VM, key).
	IdempotencyKey string `json:"-"`
	BodyHash       string `json:"-"`
}

// SteerRecord is how a keyed steer was delivered, so a repeat returns it.
type SteerRecord struct {
	TextHash string `json:"textHash"`
	Delivery string `json:"delivery"` // native | restart
	PromptID string `json:"promptId"` // the turn carrying the message
}

// Terminal reports whether the turn has finished.
func (p *PromptRecord) Terminal() bool {
	return p.State == "succeeded" || p.State == "failed" || p.State == "cancelled"
}

// PutConversation inserts or replaces c.
func (t *Tx) PutConversation(c *ConversationRecord) error {
	doc, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO conversations (id, vm_id, owner, created_ns, updated_ns, doc) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET updated_ns=excluded.updated_ns, doc=excluded.doc`,
		c.ID, c.VMID, c.Owner, c.CreatedAt.UnixNano(), c.UpdatedAt.UnixNano(), string(doc))
	return err
}

func scanDoc[T any](s interface{ Scan(...any) error }) (*T, error) {
	var doc string
	if err := s.Scan(&doc); err != nil {
		return nil, err
	}
	v := new(T)
	return v, json.Unmarshal([]byte(doc), v)
}

// GetConversation returns conversation id.
func (t *Tx) GetConversation(id string) (*ConversationRecord, error) {
	c, err := scanDoc[ConversationRecord](t.row(`SELECT doc FROM conversations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("conversation", id)
	}
	return c, err
}

// ListConversations returns one page of owner's conversations on vm in
// stable (createdAt, id) order, like every other list.
func (t *Tx) ListConversations(owner domain.PrincipalID, vm domain.VMID, p Page) ([]*ConversationRecord, string, error) {
	c, err := p.decode()
	if err != nil {
		return nil, "", err
	}
	rows, err := t.query(`SELECT doc, created_ns, id FROM conversations WHERE owner = ? AND vm_id = ? AND (created_ns, id) > (?, ?)
ORDER BY created_ns, id LIMIT ?`, owner, vm, c.CreatedNS, c.ID, p.Limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []*ConversationRecord{}
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
		cr, err := scanDoc[ConversationRecord](staticRow{doc})
		if err != nil {
			return nil, "", err
		}
		out = append(out, cr)
		lastNS, lastID = ns, id
	}
	return out, "", rows.Err()
}

// RecentConversations returns owner's conversations on vm, most recently
// active first (used to resume the latest conversation by default).
func (t *Tx) RecentConversations(owner domain.PrincipalID, vm domain.VMID, limit int) ([]*ConversationRecord, error) {
	rows, err := t.query(`SELECT doc FROM conversations WHERE owner = ? AND vm_id = ? ORDER BY updated_ns DESC, id DESC LIMIT ?`, owner, vm, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ConversationRecord{}
	for rows.Next() {
		c, err := scanDoc[ConversationRecord](rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PutPrompt inserts or replaces p.
func (t *Tx) PutPrompt(p *PromptRecord) error {
	doc, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO prompts (id, conversation_id, state, created_ns, doc, owner, vm_id, idempotency_key, body_hash)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET state=excluded.state, doc=excluded.doc`,
		p.ID, p.ConversationID, p.State, p.CreatedAt.UnixNano(), string(doc), p.Owner, p.VMID, nullString(p.IdempotencyKey), p.BodyHash)
	if err != nil && isUnique(err) {
		return domain.Errorf(domain.CodeIdempotencyConflict, "Idempotency-Key was already used for another prompt")
	}
	return err
}

// PromptByKey returns the prompt created with (owner, vm, key), or nil.
func (t *Tx) PromptByKey(owner domain.PrincipalID, vm domain.VMID, key string) (*PromptRecord, error) {
	var doc, hash string
	err := t.row(`SELECT doc, body_hash FROM prompts WHERE owner = ? AND vm_id = ? AND idempotency_key = ?`, owner, vm, key).Scan(&doc, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p, err := scanDoc[PromptRecord](staticRow{doc})
	if err != nil {
		return nil, err
	}
	p.IdempotencyKey, p.BodyHash = key, hash
	return p, nil
}

// GetPrompt returns prompt id.
func (t *Tx) GetPrompt(id string) (*PromptRecord, error) {
	p, err := scanDoc[PromptRecord](t.row(`SELECT doc FROM prompts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("prompt", id)
	}
	return p, err
}

// UnfinishedPrompts lists turns still starting or running (for recovery).
func (t *Tx) UnfinishedPrompts() ([]*PromptRecord, error) {
	rows, err := t.query(`SELECT doc FROM prompts WHERE state IN ('starting', 'running') ORDER BY created_ns`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PromptRecord
	for rows.Next() {
		p, err := scanDoc[PromptRecord](rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AppendPromptEvent stores event seq of prompt (doc is its JSON).
func (t *Tx) AppendPromptEvent(prompt string, seq int64, doc []byte) error {
	_, err := t.exec(`INSERT INTO prompt_events (prompt_id, seq, doc) VALUES (?, ?, ?)`, prompt, seq, string(doc))
	return err
}

// PromptEvents returns up to limit event docs of prompt after seq.
func (t *Tx) PromptEvents(prompt string, after int64, limit int) ([]json.RawMessage, error) {
	rows, err := t.query(`SELECT doc FROM prompt_events WHERE prompt_id = ? AND seq > ? ORDER BY seq LIMIT ?`, prompt, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(doc))
	}
	return out, rows.Err()
}

// FeedEvent is one stored prompt event with its position in the VM-wide
// feed (the event row's insertion order).
type FeedEvent struct {
	Pos int64
	Doc json.RawMessage
}

// PromptFeed returns up to limit events of owner's turns on vm after
// position after, in insertion order; conversation, when set, narrows it to
// one conversation. Prompts and their events are never deleted while the
// VM exists, so positions only grow.
func (t *Tx) PromptFeed(owner domain.PrincipalID, vm domain.VMID, conversation string, after int64, limit int) ([]FeedEvent, error) {
	rows, err := t.query(`SELECT e.rowid, e.doc FROM prompt_events e JOIN prompts p ON p.id = e.prompt_id
WHERE e.rowid > ? AND p.owner = ? AND p.vm_id = ? AND (? = '' OR p.conversation_id = ?) ORDER BY e.rowid LIMIT ?`,
		after, owner, vm, conversation, conversation, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FeedEvent{}
	for rows.Next() {
		var e FeedEvent
		var doc string
		if err := rows.Scan(&e.Pos, &doc); err != nil {
			return nil, err
		}
		e.Doc = json.RawMessage(doc)
		out = append(out, e)
	}
	return out, rows.Err()
}
