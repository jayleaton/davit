package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"tailscale.com/client/tailscale/apitype"

	"davit/api"
	"davit/internal/domain"
)

// Identity is the tailnet principal behind one accepted connection, as
// reported by the local tailscale backend for the actual peer address.
type Identity struct {
	// UserID and LoginName identify the human (or the tag owner for tagged
	// nodes). Map principals by UserID/NodeID, not by display strings.
	UserID    int64
	LoginName string
	// NodeID is the stable tailnet node ID of the peer device.
	NodeID   string
	NodeName string
	// Tags is non-empty for tagged (workload) nodes; tagged nodes are not
	// human users and need explicit grants.
	Tags []string
	// Addr is the peer address the identity was resolved for.
	Addr string
}

// Tagged reports whether the peer is a tagged node.
func (id Identity) Tagged() bool { return len(id.Tags) > 0 }

func (id Identity) String() string {
	if id.Tagged() {
		return fmt.Sprintf("node %s (%s) tags=%s", id.NodeName, id.NodeID, strings.Join(id.Tags, ","))
	}
	return fmt.Sprintf("%s via %s (%s)", id.LoginName, id.NodeName, id.NodeID)
}

// WhoIser resolves a peer address to its tailnet identity. *local.Client
// from tsnet.Server.LocalClient satisfies it.
type WhoIser interface {
	WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)
}

// Identify resolves the identity of addr, failing closed on any error or
// incomplete answer.
func Identify(ctx context.Context, w WhoIser, addr string) (Identity, error) {
	if w == nil || addr == "" {
		return Identity{}, ErrUnidentified
	}
	res, err := w.WhoIs(ctx, addr)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrUnidentified, err)
	}
	if res == nil || res.Node == nil || res.UserProfile == nil {
		return Identity{}, ErrUnidentified
	}
	n := res.Node
	id := Identity{
		UserID:    int64(res.UserProfile.ID),
		LoginName: res.UserProfile.LoginName,
		NodeID:    string(n.StableID),
		NodeName:  strings.TrimSuffix(n.Name, "."),
		Tags:      append([]string(nil), n.Tags...),
		Addr:      addr,
	}
	if id.NodeID == "" || (id.UserID == 0 && !id.Tagged()) {
		return Identity{}, ErrUnidentified
	}
	return id, nil
}

// IdentifyConn resolves the identity of an accepted connection's peer.
func IdentifyConn(ctx context.Context, w WhoIser, c net.Conn) (Identity, error) {
	if c == nil || c.RemoteAddr() == nil {
		return Identity{}, ErrUnidentified
	}
	return Identify(ctx, w, c.RemoteAddr().String())
}

type identityKey struct{}

// WithIdentity returns ctx carrying id.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFromContext returns the identity attached by the gateway. ok is
// false for requests that did not arrive through it.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// validRequestID accepts a client-chosen request ID only if it is short
// and plain, as the API itself does.
func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// RequireIdentity wraps next so that it only ever sees requests whose
// transport peer resolved to a tailnet identity, which it finds in the request
// context. r.RemoteAddr is the address of the accepted tsnet connection, not a
// header, so proxies and forged X-Forwarded-For cannot change it.
func RequireIdentity(w WhoIser, next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		id, err := Identify(r.Context(), w, r.RemoteAddr)
		if err != nil {
			// The same ErrorResponse shape as every other API error, so
			// clients map it to unauthenticated (CLI exit 3).
			rid := r.Header.Get(api.HeaderRequestID)
			if !validRequestID(rid) {
				var b [8]byte
				rand.Read(b[:])
				rid = "req_" + hex.EncodeToString(b[:])
			}
			rw.Header().Set(api.HeaderRequestID, rid)
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(rw).Encode(api.ErrorResponse{Error: api.Error{
				Code: domain.CodeUnauthenticated, Message: "tailnet identity required", RequestID: rid}})
			return
		}
		next.ServeHTTP(rw, r.WithContext(WithIdentity(r.Context(), id)))
	})
}
