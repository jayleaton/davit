package gateway

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// RFB (VNC) proxying for desktop endpoints (G15). The gateway completes the
// RFB handshake with each side itself: with the guest's VNC server as a
// client (security None: the server listens on guest loopback, reachable
// only through the guest agent), and with the viewer as a server offering
// only None (the viewer was authenticated by its tailnet identity before the
// WebSocket opened). Afterwards the guest's stream is copied verbatim, and
// the viewer's messages are parsed one by one: a view-only viewer's key,
// pointer and clipboard messages are dropped here, never trusting the guest
// to enforce it, and anything unparsable ends the session (fail closed).

const (
	rfbHandshakeTimeout = 15 * time.Second
	rfbMaxName          = 1 << 16
	rfbMaxCutText       = 1 << 20
	rfbSecurityNone     = 1
)

// RFB client-to-server message types.
const (
	rfbSetPixelFormat           = 0
	rfbSetEncodings             = 2
	rfbFramebufferUpdateRequest = 3
	rfbKeyEvent                 = 4
	rfbPointerEvent             = 5
	rfbClientCutText            = 6
	rfbEnableContinuousUpdates  = 150
	rfbClientFence              = 248
)

var errRFB = errors.New("gateway: rfb")

func rfbErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errRFB, fmt.Sprintf(format, args...))
}

// rfbMinor parses "RFB 003.00x\n" and returns the minor version the
// gateway speaks with that peer (3, 7 or 8).
func rfbMinor(v []byte) (int, error) {
	if len(v) != 12 || string(v[:4]) != "RFB " || v[7] != '.' || v[11] != '\n' {
		return 0, rfbErr("not an RFB version %q", v)
	}
	major, err1 := strconv.Atoi(string(v[4:7]))
	minor, err2 := strconv.Atoi(string(v[8:11]))
	if err1 != nil || err2 != nil || major != 3 {
		return 0, rfbErr("unsupported RFB version %q", v)
	}
	switch {
	case minor >= 8:
		return 8, nil
	case minor == 7:
		return 7, nil
	}
	return 3, nil
}

func rfbVersion(minor int) []byte { return fmt.Appendf(nil, "RFB 003.%03d\n", minor) }

// rfbReason reads a failure reason (u32 length + text).
func rfbReason(r io.Reader) string {
	var n uint32
	if binary.Read(r, binary.BigEndian, &n) != nil || n > 4096 {
		return "(no reason)"
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "(no reason)"
	}
	return string(b)
}

// rfbGuestHandshake runs the client side against the guest's VNC server and
// returns its ServerInit message (to replay to viewers).
func rfbGuestHandshake(g *bufio.Reader, gw io.Writer) ([]byte, error) {
	v := make([]byte, 12)
	if _, err := io.ReadFull(g, v); err != nil {
		return nil, rfbErr("guest version: %v", err)
	}
	minor, err := rfbMinor(v)
	if err != nil {
		return nil, err
	}
	if _, err := gw.Write(rfbVersion(minor)); err != nil {
		return nil, err
	}
	if minor == 3 {
		var t uint32
		if err := binary.Read(g, binary.BigEndian, &t); err != nil {
			return nil, rfbErr("guest security: %v", err)
		}
		switch t {
		case rfbSecurityNone:
		case 0:
			return nil, rfbErr("guest refused: %s", rfbReason(g))
		default:
			return nil, rfbErr("the guest VNC server requires authentication (type %d); run it without a password on guest loopback", t)
		}
	} else {
		n, err := g.ReadByte()
		if err != nil {
			return nil, rfbErr("guest security: %v", err)
		}
		if n == 0 {
			return nil, rfbErr("guest refused: %s", rfbReason(g))
		}
		types := make([]byte, n)
		if _, err := io.ReadFull(g, types); err != nil {
			return nil, rfbErr("guest security types: %v", err)
		}
		ok := false
		for _, t := range types {
			ok = ok || t == rfbSecurityNone
		}
		if !ok {
			return nil, rfbErr("the guest VNC server offers no security None (types %v); run it without a password on guest loopback", types)
		}
		if _, err := gw.Write([]byte{rfbSecurityNone}); err != nil {
			return nil, err
		}
		if minor == 8 {
			var res uint32
			if err := binary.Read(g, binary.BigEndian, &res); err != nil {
				return nil, rfbErr("guest security result: %v", err)
			}
			if res != 0 {
				return nil, rfbErr("guest security failed: %s", rfbReason(g))
			}
		}
	}
	if _, err := gw.Write([]byte{1}); err != nil { // ClientInit: shared
		return nil, err
	}
	head := make([]byte, 24)
	if _, err := io.ReadFull(g, head); err != nil {
		return nil, rfbErr("guest ServerInit: %v", err)
	}
	nameLen := binary.BigEndian.Uint32(head[20:24])
	if nameLen > rfbMaxName {
		return nil, rfbErr("guest desktop name too long (%d)", nameLen)
	}
	name := make([]byte, nameLen)
	if _, err := io.ReadFull(g, name); err != nil {
		return nil, rfbErr("guest desktop name: %v", err)
	}
	return append(head, name...), nil
}

// rfbViewerHandshake runs the server side with the viewer, offering only
// security None, and sends serverInit.
func rfbViewerHandshake(v *bufio.Reader, vw io.Writer, serverInit []byte) error {
	if _, err := vw.Write(rfbVersion(8)); err != nil {
		return err
	}
	ver := make([]byte, 12)
	if _, err := io.ReadFull(v, ver); err != nil {
		return rfbErr("viewer version: %v", err)
	}
	minor, err := rfbMinor(ver)
	if err != nil {
		return err
	}
	if minor == 3 {
		if err := binary.Write(vw, binary.BigEndian, uint32(rfbSecurityNone)); err != nil {
			return err
		}
	} else {
		if _, err := vw.Write([]byte{1, rfbSecurityNone}); err != nil {
			return err
		}
		t, err := v.ReadByte()
		if err != nil {
			return rfbErr("viewer security: %v", err)
		}
		if t != rfbSecurityNone {
			return rfbErr("viewer chose security type %d", t)
		}
		if minor == 8 {
			if err := binary.Write(vw, binary.BigEndian, uint32(0)); err != nil {
				return err
			}
		}
	}
	if _, err := v.ReadByte(); err != nil { // ClientInit (the guest side is always shared)
		return rfbErr("viewer ClientInit: %v", err)
	}
	_, err = vw.Write(serverInit)
	return err
}

// rfbViewerMessages relays the viewer's messages to the guest, dropping
// input when viewOnly. It returns when either side fails.
func rfbViewerMessages(v *bufio.Reader, gw io.Writer, viewOnly bool, onDrop func()) error {
	buf := make([]byte, 0, 64)
	read := func(n int) ([]byte, error) {
		buf = buf[:n]
		_, err := io.ReadFull(v, buf)
		return buf, err
	}
	for {
		t, err := v.ReadByte()
		if err != nil {
			return err
		}
		var body []byte
		input := false
		switch t {
		case rfbSetPixelFormat:
			body, err = read(19)
		case rfbSetEncodings:
			var h []byte
			if h, err = read(3); err == nil {
				n := int(binary.BigEndian.Uint16(h[1:3]))
				enc := make([]byte, 3+4*n)
				copy(enc, h)
				_, err = io.ReadFull(v, enc[3:])
				body = enc
			}
		case rfbFramebufferUpdateRequest:
			body, err = read(9)
		case rfbKeyEvent:
			input = true
			body, err = read(7)
		case rfbPointerEvent:
			input = true
			body, err = read(5)
		case rfbClientCutText:
			input = true
			var h []byte
			if h, err = read(7); err == nil {
				n := binary.BigEndian.Uint32(h[3:7])
				if n > rfbMaxCutText {
					return rfbErr("viewer clipboard text too long (%d)", n)
				}
				txt := make([]byte, 7+int(n))
				copy(txt, h)
				_, err = io.ReadFull(v, txt[7:])
				body = txt
			}
		case rfbEnableContinuousUpdates:
			body, err = read(9)
		case rfbClientFence:
			var h []byte
			if h, err = read(8); err == nil {
				n := int(h[7])
				f := make([]byte, 8+n)
				copy(f, h)
				_, err = io.ReadFull(v, f[8:])
				body = f
			}
		default:
			return rfbErr("unknown viewer message type %d", t)
		}
		if err != nil {
			return err
		}
		if input && viewOnly {
			if onDrop != nil {
				onDrop()
			}
			continue
		}
		msg := append([]byte{t}, body...)
		if _, err := gw.Write(msg); err != nil {
			return err
		}
	}
}

// ProxyRFB connects a viewer (a WebSocket byte stream) to the guest's VNC
// server. viewOnly drops the viewer's input; onDrop counts each drop.
func ProxyRFB(viewer net.Conn, guest net.Conn, viewOnly bool, onDrop func()) error {
	defer viewer.Close()
	defer guest.Close()
	deadline := time.Now().Add(rfbHandshakeTimeout)
	guest.SetDeadline(deadline)
	viewer.SetDeadline(deadline)
	g := bufio.NewReader(guest)
	init, err := rfbGuestHandshake(g, guest)
	if err != nil {
		return err
	}
	v := bufio.NewReader(viewer)
	if err := rfbViewerHandshake(v, viewer, init); err != nil {
		return err
	}
	guest.SetDeadline(time.Time{})
	viewer.SetDeadline(time.Time{})
	done := make(chan error, 2)
	go func() { _, err := io.Copy(viewer, g); done <- err }()
	go func() { done <- rfbViewerMessages(v, guest, viewOnly, onDrop) }()
	err = <-done
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return err
}
