package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
	"davit/internal/wsock"
)

// reverseSpecRE matches GUESTPORT:HOST:PORT (or GUESTPORT:PORT for
// 127.0.0.1); HOST may be a [bracketed] IPv6 address.
var reverseSpecRE = regexp.MustCompile(`^(\d{1,5}):(?:(\[[^\]]+\]|[^:\[\]]+):)?(\d{1,5})$`)

// localDialTimeout bounds the dial of the local service per guest
// connection.
const localDialTimeout = 10 * time.Second

func reversePath(vm string) string { return vmPath(vm) + "/reverse-forwards" }

// runReverse is `forward --reverse`: a guest loopback port whose connections
// reach a service this machine can reach (held open until Ctrl-C), or with
// --host-service an operator-listed host service; --list and --revoke manage
// a VM's reverse forwards.
func runReverse(ctx context.Context, s *session, o *opts, args []string) error {
	f := &o.ssh
	switch {
	case f.reverseList:
		return reverseList(ctx, s, args)
	case f.reverseRevoke != "":
		return reverseRevoke(ctx, s, f.reverseRevoke, args)
	}
	usage := "forward --reverse [ID] GUESTPORT:HOST:PORT"
	if len(args) == 0 {
		return usageErr("%s %s", buildinfo.Name, usage)
	}
	m := reverseSpecRE.FindStringSubmatch(args[len(args)-1])
	if m == nil {
		return usageErr("%s %s (e.g. 8080:localhost:3000)", buildinfo.Name, usage)
	}
	if err := maxArgs(args[:len(args)-1], 1, usage); err != nil {
		return err
	}
	guestPort, _ := strconv.Atoi(m[1])
	port, _ := strconv.Atoi(m[3])
	host := m[2]
	if host == "" {
		host = "127.0.0.1"
	}
	if len(host) > 1 && host[0] == '[' {
		host = host[1 : len(host)-1]
	}
	if guestPort < 1 || guestPort > 65535 || port < 1 || port > 65535 {
		return usageErr("ports must be 1..65535: %s %s", buildinfo.Name, usage)
	}
	local := net.JoinHostPort(host, strconv.Itoa(port))
	ref, err := s.currentID(args[:len(args)-1])
	if err != nil {
		return err
	}
	vm, err := s.getVM(ctx, ref)
	if err != nil {
		return err
	}
	if f.hostService {
		var fw api.ReverseForward
		req := api.CreateReverseForwardRequest{GuestPort: guestPort, Target: api.ReverseTarget{Kind: api.ReverseTargetHost, Address: local}}
		if err := s.cl.do(ctx, http.MethodPost, reversePath(string(vm.ID)), nil, req, nil, &fw); err != nil {
			return err
		}
		s.out.human("guest 127.0.0.1:%d on %s -> host service %s (%s; revoke with `%s forward --reverse --revoke %s`)",
			guestPort, vm.Name, fw.Target.Address, fw.ID, buildinfo.Name, fw.ID)
		s.out.result("", fw)
		return nil
	}
	return reverseSession(ctx, s, vm, guestPort, local)
}

func reverseList(ctx context.Context, s *session, args []string) error {
	if err := maxArgs(args, 1, "forward --reverse --list [ID]"); err != nil {
		return err
	}
	ref, err := s.currentID(args)
	if err != nil {
		return err
	}
	var l api.ReverseForwardList
	if err := s.cl.get(ctx, reversePath(ref), nil, &l); err != nil {
		return err
	}
	for _, fw := range l.Items {
		s.out.emit("item", "", 0, fw, nil)
		s.out.human("%s  guest :%d -> %s %s  %s  %d active", fw.ID, fw.GuestPort, fw.Target.Kind, fw.Target.Address, fw.State, fw.Active)
	}
	if len(l.Items) == 0 {
		s.out.human("no reverse forwards")
	}
	s.out.result("", map[string]any{"count": len(l.Items)})
	return nil
}

func reverseRevoke(ctx context.Context, s *session, id string, args []string) error {
	if err := maxArgs(args, 1, "forward --reverse --revoke FORWARD_ID [ID]"); err != nil {
		return err
	}
	ref, err := s.currentID(args)
	if err != nil {
		return err
	}
	if err := s.cl.do(ctx, http.MethodDelete, reversePath(ref)+"/"+url.PathEscape(id), nil, nil, nil, nil); err != nil {
		return err
	}
	s.out.human("revoked %s", id)
	s.out.result("", map[string]any{"id": id, "revoked": true})
	return nil
}

// reverseSession creates a session forward, holds its session open and
// bridges every announced guest connection to local.
func reverseSession(ctx context.Context, s *session, vm api.VM, guestPort int, local string) error {
	var fw api.ReverseForward
	req := api.CreateReverseForwardRequest{GuestPort: guestPort, Target: api.ReverseTarget{Kind: api.ReverseTargetSession, Address: local}}
	if err := s.cl.do(ctx, http.MethodPost, reversePath(string(vm.ID)), nil, req, nil, &fw); err != nil {
		return err
	}
	base := reversePath(string(vm.ID)) + "/" + url.PathEscape(fw.ID)
	ws, err := s.cl.websocket(ctx, api.BasePath+base+"/session")
	if err != nil {
		_ = s.cl.do(context.WithoutCancel(ctx), http.MethodDelete, base, nil, nil, nil, nil)
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	interrupted := context.AfterFunc(ctx, func() { ws.Close(wsock.CloseNormal, "") })
	defer interrupted()

	type listening struct {
		VMID      domain.VMID `json:"vmId"`
		ForwardID string      `json:"forwardId"`
		GuestPort int         `json:"guestPort"`
		Local     string      `json:"local"`
	}
	var seq atomic.Int64
	s.out.emit("reverse.listening", "", seq.Add(1), listening{vm.ID, fw.ID, guestPort, local}, nil)
	s.out.human("forwarding guest 127.0.0.1:%d on %s -> %s (Ctrl-C to stop)", guestPort, vm.Name, local)

	type connEvent struct {
		ConnectionID string `json:"connectionId"`
		BytesIn      int64  `json:"bytesIn,omitempty"`  // guest -> local service
		BytesOut     int64  `json:"bytesOut,omitempty"` // local service -> guest
	}
	bctx, cancelBridges := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelBridges()
	var wg sync.WaitGroup
	var conns atomic.Int64
	bridge := func(cid string) {
		defer wg.Done()
		st, err := s.cl.websocket(bctx, api.BasePath+base+"/connections/"+url.PathEscape(cid))
		if err != nil {
			e := asErrorBody(err)
			s.out.emit("reverse.error", "", seq.Add(1), connEvent{ConnectionID: cid}, &e)
			s.out.diag("reverse %s: %s", cid, e.Message)
			return
		}
		stream := wsock.NewStream(st, 30*time.Second)
		defer stream.Close()
		d := net.Dialer{Timeout: localDialTimeout}
		c, err := d.DialContext(bctx, "tcp", local)
		if err != nil {
			e := ErrorBody{Code: domain.CodeUnavailable, Message: "dial " + local + ": " + err.Error(), Origin: "client"}
			s.out.emit("reverse.error", "", seq.Add(1), connEvent{ConnectionID: cid}, &e)
			s.out.diag("reverse %s: %s", cid, e.Message)
			return
		}
		conns.Add(1)
		s.out.emit("reverse.connection", "", seq.Add(1), connEvent{ConnectionID: cid}, nil)
		in, out := spliceHalfClose(bctx, c, stream)
		s.out.emit("reverse.closed", "", seq.Add(1), connEvent{ConnectionID: cid, BytesIn: in, BytesOut: out}, nil)
	}

	reason := ""
	for {
		typ, data, err := ws.ReadMessage()
		if err != nil {
			if reason == "" && ctx.Err() == nil {
				reason = "session lost: " + err.Error()
			}
			break
		}
		if typ != wsock.TextMessage {
			continue
		}
		var ev api.ReverseSessionEvent
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		switch ev.Type {
		case api.ReverseEventConnection:
			wg.Add(1)
			go bridge(ev.ConnectionID)
		case api.ReverseEventRevoked:
			reason = ev.Reason
		}
	}
	ws.Close(wsock.CloseNormal, "")
	cancelBridges() // the forward is gone: so are its connections
	wg.Wait()
	if ctx.Err() != nil && reason == "" {
		// Ctrl-C: closing the session revoked the forward; make sure.
		_ = s.cl.do(context.WithoutCancel(ctx), http.MethodDelete, base, nil, nil, nil, nil)
		s.out.result("", map[string]any{"vmId": vm.ID, "forwardId": fw.ID, "guestPort": guestPort, "local": local, "connections": conns.Load()})
		return nil
	}
	e := clientErr(domain.CodeUnavailable, "reverse forward %s ended: %s", fw.ID, reason)
	e.Details = map[string]any{"forwardId": fw.ID, "reason": reason, "connections": conns.Load()}
	return e
}

// spliceHalfClose copies local <-> stream with half-close both ways and
// returns the bytes moved (guest -> local, local -> guest). ctx ending cuts
// both.
func spliceHalfClose(ctx context.Context, local net.Conn, stream *wsock.Stream) (in, out int64) {
	stop := context.AfterFunc(ctx, func() { local.Close(); stream.Close() })
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, _ = io.Copy(stream, local)
		_ = stream.CloseWrite()
	}()
	var err error
	in, err = io.Copy(local, stream)
	if cw, ok := local.(interface{ CloseWrite() error }); ok && err == nil && !stream.PeerClosed() {
		_ = cw.CloseWrite()
	} else {
		local.Close()
	}
	<-done
	local.Close()
	stream.Close()
	return in, out
}
