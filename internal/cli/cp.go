package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
	"davit/internal/tarx"
)

// cp copies files between this machine and a VM through the guest agent
// (boat-style `VM:/path`). `scp` stays reserved for the SSH/gateway path.

type cpOpts struct {
	recursive bool
	user      string
}

func cpFlags(fs *flag.FlagSet, o *opts) {
	fs.BoolVar(&o.cp.recursive, "r", false, "copy a directory (tar through the agent, modes and symlinks kept)")
	fs.BoolVar(&o.cp.recursive, "recursive", false, "same as -r")
	fs.StringVar(&o.cp.user, "user", "", "guest user whose permissions scope the copy (default: the image's)")
}

// remoteSpec splits "VM:/path". Paths starting with /, ./ or ../, and
// arguments whose ":" comes after a "/", are local.
func remoteSpec(arg string) (vm, p string, ok bool) {
	if strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../") {
		return "", "", false
	}
	vm, p, ok = strings.Cut(arg, ":")
	if !ok || vm == "" || strings.Contains(vm, "/") {
		return "", "", false
	}
	return vm, p, true
}

type cpResult struct {
	Source      string        `json:"source"`
	Destination string        `json:"destination"`
	Bytes       int64         `json:"bytes"`
	Recursive   bool          `json:"recursive"`
	File        *api.FileInfo `json:"file,omitempty"`
}

func runCp(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) != 2 {
		return usageErr("usage: %s cp [-r] SRC DST, with exactly one side VM:/path", buildinfo.Name)
	}
	srcVM, srcPath, srcRemote := remoteSpec(args[0])
	dstVM, dstPath, dstRemote := remoteSpec(args[1])
	if srcRemote == dstRemote {
		return usageErr("exactly one of SRC and DST must be VM:/path")
	}
	vmRef, remote := srcVM, srcPath
	if dstRemote {
		vmRef, remote = dstVM, dstPath
	}
	if !strings.HasPrefix(remote, "/") {
		return usageErr("the VM path must be absolute: %s:/path", vmRef)
	}
	var vm api.VM
	if err := s.cl.get(ctx, vmPath(vmRef), nil, &vm); err != nil {
		return err
	}
	p := &progress{s: s, start: time.Now()}
	stop := p.run()
	var res cpResult
	var err error
	if srcRemote {
		res, err = s.download(ctx, o.cp, vm, remote, args[1], p)
	} else {
		res, err = s.upload(ctx, o.cp, vm, args[0], remote, p)
	}
	stop()
	if err != nil {
		return err
	}
	res.Source, res.Recursive = args[0], o.cp.recursive
	s.out.result("", res)
	s.out.diag("copied %s to %s (%s in %s)", res.Source, res.Destination, humanBytes(res.Bytes), time.Since(p.start).Round(time.Millisecond))
	return nil
}

func fileQuery(vm api.VM, p, user string, archive bool) url.Values {
	q := url.Values{"path": {p}, "generation": {strconv.FormatInt(vm.InstanceGeneration, 10)}}
	if user != "" {
		q.Set("user", user)
	}
	if archive {
		q.Set("archive", api.ArchiveTar)
	}
	return q
}

// stream sends a raw request and returns a 2xx response for the caller to
// read, or the decoded API error.
func (c *client) stream(ctx context.Context, method, p string, q url.Values, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+p+"?"+q.Encode(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(api.HeaderRequestID, c.requestID)
	c.authorize(req.Header)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, clientErr(domain.CodeTimeout, "timed out")
		}
		e := clientErr(domain.CodeUnavailable, "transfer failed: %v", err)
		e.Retryable = true
		return nil, e
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var er api.ErrorResponse
	if json.Unmarshal(data, &er) != nil || er.Error.Code == "" {
		return nil, &cliError{ErrorBody{Code: domain.CodeInternal, Origin: "server", Message: fmt.Sprintf("unexpected HTTP %d from daemon", resp.StatusCode)}}
	}
	return nil, &cliError{ErrorBody{Code: er.Error.Code, Message: er.Error.Message, Retryable: er.Error.Retryable,
		Details: er.Error.Details, Origin: "server", RequestID: er.Error.RequestID}}
}

// localTarget resolves DST like cp: into an existing directory (or one
// named with a trailing slash) under name, else DST itself.
func localTarget(dst, name string) string {
	if fi, err := os.Stat(dst); (err == nil && fi.IsDir()) || strings.HasSuffix(dst, string(filepath.Separator)) {
		return filepath.Join(dst, name)
	}
	return dst
}

func (s *session) download(ctx context.Context, o cpOpts, vm api.VM, remote, dst string, p *progress) (cpResult, error) {
	resp, err := s.cl.stream(ctx, http.MethodGet, vmPath(string(vm.ID))+"/files", fileQuery(vm, remote, o.user, o.recursive), nil, "")
	if err != nil {
		return cpResult{}, err
	}
	defer resp.Body.Close()
	mode := fs.FileMode(0o644)
	if m, err := strconv.ParseUint(resp.Header.Get(api.HeaderFileMode), 8, 32); err == nil {
		mode = fs.FileMode(m).Perm()
	}
	target := localTarget(dst, path.Base(remote))
	body := p.reader(resp.Body)
	if o.recursive {
		if err := tarx.Extract(body, target, mode); err != nil {
			return cpResult{}, clientErr(domain.CodeUnavailable, "extracting into %s: %v", target, err)
		}
		return cpResult{Destination: target, Bytes: p.n.Load()}, nil
	}
	if err := writeLocal(target, mode, body, resp.ContentLength); err != nil {
		return cpResult{}, err
	}
	return cpResult{Destination: target, Bytes: p.n.Load()}, nil
}

// writeLocal atomically replaces target with exactly want bytes of r (when
// want >= 0), so a cut transfer never leaves a partial file.
func writeLocal(target string, mode fs.FileMode, r io.Reader, want int64) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".davit-cp-*")
	if err != nil {
		return clientErr(domain.CodeInvalidArgument, "cannot write %s: %v", target, err)
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, r)
	if err == nil && want >= 0 && n != want {
		err = fmt.Errorf("got %d of %d bytes", n, want)
	}
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return clientErr(domain.CodeUnavailable, "transfer of %s failed: %v", target, err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return clientErr(domain.CodeInvalidArgument, "cannot write %s: %v", target, err)
	}
	return nil
}

func (s *session) upload(ctx context.Context, o cpOpts, vm api.VM, src, remote string, p *progress) (cpResult, error) {
	fi, err := os.Lstat(src)
	if err != nil {
		return cpResult{}, clientErr(domain.CodeNotFound, "%v", err)
	}
	if strings.HasSuffix(remote, "/") {
		remote = path.Join(remote, filepath.Base(src))
	}
	var body io.Reader
	var q url.Values
	ct := "application/octet-stream"
	switch {
	case fi.IsDir() && !o.recursive:
		return cpResult{}, usageErr("%s is a directory: use -r", src)
	case o.recursive:
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(tarx.Write(pw, src, fi)) }()
		defer pr.Close()
		body, q, ct = pr, fileQuery(vm, remote, o.user, true), api.ContentTypeTar
	case fi.Mode().IsRegular():
		f, err := os.Open(src)
		if err != nil {
			return cpResult{}, clientErr(domain.CodeNotFound, "%v", err)
		}
		defer f.Close()
		body, q = f, fileQuery(vm, remote, o.user, false)
	default:
		return cpResult{}, usageErr("%s is not a regular file or directory", src)
	}
	if fi.IsDir() || fi.Mode().IsRegular() {
		q.Set("mode", "0"+strconv.FormatUint(uint64(fi.Mode().Perm()), 8))
	}
	resp, err := s.cl.stream(ctx, http.MethodPut, vmPath(string(vm.ID))+"/files", q, p.reader(body), ct)
	if err != nil {
		return cpResult{}, err
	}
	defer resp.Body.Close()
	var info api.FileInfo
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&info); err != nil {
		return cpResult{}, clientErr(domain.CodeInternal, "decoding response: %v", err)
	}
	return cpResult{Destination: string(vm.ID) + ":" + remote, Bytes: info.SizeBytes, File: &info}, nil
}

// progress counts transferred bytes and reports them every half second:
// a `progress` event in --json mode, a rewritten stderr line otherwise.
type progress struct {
	s     *session
	start time.Time
	n     atomic.Int64
	seq   int64
}

type countReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n.Add(int64(n))
	return n, err
}

func (p *progress) reader(r io.Reader) io.Reader { return countReader{r, &p.n} }

func (p *progress) run() (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		shown := false
		for {
			select {
			case <-done:
				if shown && !p.s.g.json {
					fmt.Fprintln(p.s.io.Stderr)
				}
				return
			case <-t.C:
				p.seq++
				n := p.n.Load()
				p.s.out.emit("progress", "", p.seq, map[string]int64{"bytes": n}, nil)
				if !p.s.g.json {
					fmt.Fprintf(p.s.io.Stderr, "\r%s", humanBytes(n))
					shown = true
				}
			}
		}
	}()
	return func() { close(done); <-finished }
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
