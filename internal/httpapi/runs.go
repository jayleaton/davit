package httpapi

import (
	"archive/tar"
	"io"
	"net/http"
	"path"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestexec"
	"davit/internal/runs"
)

// SetRuns enables the run endpoints (createRun, getRun, startRun,
// cancelRun). Without it they answer 422 unsupported_capability: runs need
// the guest process API.
func (s *Server) SetRuns(r *runs.Service) { s.runs = r }

func (s *Server) runService() (*runs.Service, error) {
	if s.runs == nil {
		return nil, domain.Errorf(domain.CodeUnsupportedCapability, "this runtime has no guest transport").
			WithDetail("capability", "guestTransport")
	}
	return s.runs, nil
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) error {
	rs, err := s.runService()
	if err != nil {
		return err
	}
	var body api.RunRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	run, err := rs.CreateRun(r.Context(), principal(r), r.Header.Get(api.HeaderIdempotencyKey), body)
	if err != nil {
		return err
	}
	w.Header().Set("Location", api.BasePath+"/runs/"+run.ID)
	return writeJSON(w, http.StatusCreated, run)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) error {
	rs, err := s.runService()
	if err != nil {
		return err
	}
	run, err := rs.GetRun(r.Context(), principal(r), r.PathValue("runId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, run)
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) error {
	rs, err := s.runService()
	if err != nil {
		return err
	}
	run, err := rs.StartRun(r.Context(), principal(r), r.PathValue("runId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, run)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) error {
	rs, err := s.runService()
	if err != nil {
		return err
	}
	run, err := rs.CancelRun(r.Context(), principal(r), r.PathValue("runId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, run)
}

// pushTree applies a tar of client-selected files under path through the
// guest archive write (writeFile with archive=tar). mode=merge adds and
// replaces files; mode=mirror would also delete files the tree lacks,
// which needs guest-side deletion that does not exist yet.
func (s *Server) pushTree(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	p := q.Get("path")
	if !path.IsAbs(p) || path.Clean(p) != p {
		return domain.Errorf(domain.CodeInvalidArgument, "path must be an absolute, clean guest directory").WithDetail("field", "path")
	}
	switch q.Get("mode") {
	case "", api.PushMerge:
	case api.PushMirror:
		return domain.NotImplemented(domain.FeaturePush, "pushTree.mode=mirror")
	default:
		return domain.Errorf(domain.CodeInvalidArgument, "mode must be %q or %q", api.PushMerge, api.PushMirror).WithDetail("field", "mode")
	}
	m, err := s.processes()
	if err != nil {
		return err
	}
	vm, err := s.svc.GetVM(r.Context(), principal(r), r.PathValue("vmId"))
	if err != nil {
		return err
	}
	fr := guestexec.FileRequest{Principal: principal(r), VM: string(vm.ID), Path: p, Generation: vm.InstanceGeneration, Archive: true}
	body, files := countEntries(r.Body)
	info, err := m.WriteFile(r.Context(), fr, 0, body)
	n := files()
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, api.PushResult{Path: p, Files: n, Bytes: info.SizeBytes, SHA256: info.SHA256,
		Generation: info.Generation})
}

// countEntries passes body through and counts the tar's file and symlink
// entries on the side; files reports the count once body is consumed (or
// abandoned).
func countEntries(body io.Reader) (io.Reader, func() int) {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	n := 0
	go func() {
		defer close(done)
		tr := tar.NewReader(pr)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			if h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeSymlink {
				n++
			}
		}
		// Whatever follows (padding, or a malformed rest the guest
		// rejects) must not stall the upload.
		_, _ = io.Copy(io.Discard, pr)
	}()
	return io.TeeReader(body, pw), func() int {
		pw.Close()
		<-done
		return n
	}
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) error {
	rs, err := s.runService()
	if err != nil {
		return err
	}
	p, err := page(r)
	if err != nil {
		return err
	}
	l, err := rs.ListRuns(r.Context(), principal(r), p.Limit, p.Cursor)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, l)
}
