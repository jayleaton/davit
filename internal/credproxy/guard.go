package credproxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

// authHeaders are the only headers the placeholder may appear in, and the
// only ones the secret is put in (SEC-5).
var authHeaders = []string{"Authorization", "X-Api-Key", "Api-Key", "X-Goog-Api-Key"}

// placeholderOutsideAuth reports the first header outside authHeaders that
// carries a placeholder.
func placeholderOutsideAuth(hdr http.Header) (string, bool) {
	for name, vs := range hdr {
		auth := false
		for _, a := range authHeaders {
			auth = auth || http.CanonicalHeaderKey(name) == a
		}
		if auth {
			continue
		}
		for _, v := range vs {
			if strings.Contains(v, placeholderPrefix) || strings.Contains(v, legacyPlaceholderPrefix) {
				return name, true
			}
		}
	}
	return "", false
}

// dotSegments reports whether an escaped path has "." or ".." segments
// (also percent-encoded) or an encoded slash or backslash (SEC-7).
func dotSegments(escaped string) bool {
	low := strings.ToLower(escaped)
	if strings.Contains(low, "%2f") || strings.Contains(low, "%5c") || strings.Contains(low, `\`) {
		return true
	}
	for _, seg := range strings.Split(low, "/") {
		seg = strings.ReplaceAll(seg, "%2e", ".")
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

const redacted = "[redacted]"

func scrubHeader(h http.Header, secret string) {
	for _, vs := range h {
		for i, v := range vs {
			vs[i] = strings.ReplaceAll(v, secret, redacted)
		}
	}
}

// redactingBody streams a response body with every occurrence of secret
// replaced, also across read boundaries (it holds back len(secret)-1
// bytes), and scrubs the response trailers once the body ends.
type redactingBody struct {
	r      io.ReadCloser
	secret []byte
	resp   *http.Response
	held   []byte // read but not yet returned
	out    []byte // ready to return
	eof    bool
}

func (b *redactingBody) Read(p []byte) (int, error) {
	for len(b.out) == 0 {
		if b.eof {
			return 0, io.EOF
		}
		buf := make([]byte, 32<<10)
		n, err := b.r.Read(buf)
		b.held = append(b.held, buf[:n]...)
		if err != nil {
			b.eof = true
			b.out = bytes.ReplaceAll(b.held, b.secret, []byte(redacted))
			b.held = nil
			if b.resp != nil {
				scrubHeader(b.resp.Trailer, string(b.secret))
			}
			if err != io.EOF {
				if len(b.out) == 0 {
					return 0, err
				}
			}
			break
		}
		replaced := bytes.ReplaceAll(b.held, b.secret, []byte(redacted))
		// Hold back only a tail that is the start of the secret (it may
		// continue in the next read); anything else streams at once, so
		// SSE events are not delayed.
		keep := partialSuffix(replaced, b.secret)
		b.out = append(b.out, replaced[:len(replaced)-keep]...)
		b.held = append([]byte(nil), replaced[len(replaced)-keep:]...)
	}
	n := copy(p, b.out)
	b.out = b.out[n:]
	return n, nil
}

func (b *redactingBody) Close() error { return b.r.Close() }

// partialSuffix is the length of the longest suffix of buf that is a
// proper prefix of secret.
func partialSuffix(buf, secret []byte) int {
	for n := min(len(secret)-1, len(buf)); n > 0; n-- {
		if bytes.Equal(buf[len(buf)-n:], secret[:n]) {
			return n
		}
	}
	return 0
}
