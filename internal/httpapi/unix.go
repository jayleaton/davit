package httpapi

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"davit/api"
	"davit/internal/domain"
)

// ListenUnix listens on a Unix socket restricted to the daemon's user: the
// directory is 0700 and the socket 0600. A stale socket left by a dead
// daemon is replaced; a live one is an error.
func ListenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("httpapi: %s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, 200*time.Millisecond); err == nil {
			c.Close()
			return nil, fmt.Errorf("httpapi: another daemon is serving %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

type peerKey struct{}

// PeerConnContext is an http.Server.ConnContext that records the Unix peer's
// OS credentials for PeerAuthenticator.
func PeerConnContext(ctx context.Context, c net.Conn) context.Context {
	if uc, ok := c.(*net.UnixConn); ok {
		if uid, err := peerUID(uc); err == nil {
			return context.WithValue(ctx, peerKey{}, uid)
		}
	}
	return ctx
}

// PeerAuthenticator authenticates local socket peers by OS credentials:
// only the daemon's own user (or root) is the owner.
type PeerAuthenticator struct {
	OwnerUID int
}

// LocalPrincipal is the principal ID of a local user.
func LocalPrincipal(uid int) domain.PrincipalID {
	return domain.PrincipalID("local:" + strconv.Itoa(uid))
}

// Identify implements Identifier: the owner over the Unix socket.
func (a PeerAuthenticator) Identify(r *http.Request) (method, identity string) {
	if uid, ok := r.Context().Value(peerKey{}).(int); ok {
		return api.AuthLocal, "uid " + strconv.Itoa(uid)
	}
	return api.AuthLocal, ""
}

func (a PeerAuthenticator) Authenticate(r *http.Request) (domain.PrincipalID, error) {
	uid, ok := r.Context().Value(peerKey{}).(int)
	if !ok {
		return "", domain.Errorf(domain.CodeUnauthenticated, "peer credentials unavailable")
	}
	if uid != a.OwnerUID && uid != 0 {
		return "", domain.Errorf(domain.CodePermissionDenied, "uid %d is not the owner", uid)
	}
	return LocalPrincipal(a.OwnerUID), nil
}
