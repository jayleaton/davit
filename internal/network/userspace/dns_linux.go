//go:build linux

package userspace

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"

	"davit/internal/network/dnsallow"
)

// dnsTimeout bounds one upstream DNS exchange.
const dnsTimeout = 3 * time.Second

// answer handles one guest DNS message and returns the response for the
// guest, or nil to drop it. Off mode drops everything; allowlist mode
// answers disallowed names NXDOMAIN and pins the addresses of allowed
// answers before the guest sees them (so its next connect is admitted).
func (st *Stack) answer(q []byte, exchange func([]byte) ([]byte, error)) []byte {
	if len(q) < 12 {
		return nil
	}
	ms := st.pol.Load()
	switch ms.mode {
	case ModeOff:
		return nil
	case ModeFiltered:
		// Only plain queries reach the host's resolvers (SEC-10).
		if !dnsallow.ValidQuery(q) {
			st.Stats.Denied.Add(1)
			return nil
		}
	case ModeAllowlist:
		if ok, refusal := ms.list.Decide(q); !ok {
			st.Stats.Denied.Add(1)
			return refusal
		}
	}
	resp, err := exchange(q)
	if err != nil || ms.mode != ModeAllowlist {
		return resp
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	if st.pol.Load() != ms {
		return nil // the policy changed under the query; the guest retries
	}
	for _, a := range dnsallow.Addresses(resp) {
		ms.pins.Add(a.Addr, a.Name, a.TTL)
	}
	return resp
}

// dnsUDP answers the datagrams of one guest UDP DNS flow until it is idle.
func (st *Stack) dnsUDP(guest *gonet.UDPConn) {
	defer guest.Close()
	var wmu sync.Mutex
	buf := make([]byte, 65535)
	for {
		_ = guest.SetReadDeadline(time.Now().Add(dnsIdle))
		n, err := guest.Read(buf)
		if err != nil {
			return
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			if resp := st.answer(q, st.exchangeUDP); resp != nil {
				wmu.Lock()
				_, _ = guest.Write(resp)
				wmu.Unlock()
			}
		}()
	}
}

// exchangeUDP asks the host resolvers in order.
func (st *Stack) exchangeUDP(q []byte) ([]byte, error) {
	err := errors.New("no resolvers")
	for _, server := range st.resolvers {
		var resp []byte
		if resp, err = exchangeUDP(server, q); err == nil {
			return resp, nil
		}
	}
	return nil, err
}

func exchangeUDP(server string, q []byte) ([]byte, error) {
	c, err := net.Dial("udp4", server)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(dnsTimeout))
	if _, err := c.Write(q); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		if n >= 12 && buf[0] == q[0] && buf[1] == q[1] {
			return buf[:n], nil
		}
	}
}

// dnsTCP answers the length-prefixed messages of one guest TCP DNS
// connection over one upstream TCP connection, in order.
func (st *Stack) dnsTCP(guest net.Conn) {
	defer guest.Close()
	var host net.Conn
	defer func() {
		if host != nil {
			host.Close()
		}
	}()
	exchange := func(q []byte) ([]byte, error) {
		if host == nil {
			var err error
			for _, server := range st.resolvers {
				if host, err = net.DialTimeout("tcp4", server, dnsTimeout); err == nil {
					break
				}
			}
			if host == nil {
				return nil, err
			}
		}
		_ = host.SetDeadline(time.Now().Add(dnsTimeout))
		err := writeMsg(host, q)
		var resp []byte
		if err == nil {
			resp, err = readMsg(host)
		}
		if err != nil {
			host.Close()
			host = nil
		}
		return resp, err
	}
	for {
		_ = guest.SetReadDeadline(time.Now().Add(dnsIdle))
		q, err := readMsg(guest)
		if err != nil {
			return
		}
		resp := st.answer(q, exchange)
		if resp == nil || writeMsg(guest, resp) != nil {
			return
		}
	}
}

func readMsg(r io.Reader) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint16(n[:]))
	_, err := io.ReadFull(r, b)
	return b, err
}

func writeMsg(w io.Writer, m []byte) error {
	b := binary.BigEndian.AppendUint16(make([]byte, 0, 2+len(m)), uint16(len(m)))
	_, err := w.Write(append(b, m...))
	return err
}
