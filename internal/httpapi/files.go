package httpapi

import (
	"io"
	"net/http"
	"strconv"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestexec"
)

// MaxUploadBytes bounds a writeFile body.
const MaxUploadBytes = 64 << 30

func fileRequest(r *http.Request) (guestexec.FileRequest, error) {
	q := r.URL.Query()
	fr := guestexec.FileRequest{Principal: principal(r), VM: r.PathValue("vmId"), Path: q.Get("path"), User: q.Get("user")}
	gen, err := strconv.ParseInt(q.Get("generation"), 10, 64)
	if err != nil || gen < 1 {
		return fr, domain.Errorf(domain.CodeInvalidArgument, "generation is required: the VM's instanceGeneration").WithDetail("field", "generation")
	}
	fr.Generation = gen
	switch q.Get("archive") {
	case "":
	case api.ArchiveTar:
		fr.Archive = true
	default:
		return fr, domain.Errorf(domain.CodeInvalidArgument, "archive must be %q", api.ArchiveTar).WithDetail("field", "archive")
	}
	return fr, nil
}

func (s *Server) readFile(w http.ResponseWriter, r *http.Request) error {
	m, err := s.processes()
	if err != nil {
		return err
	}
	fr, err := fileRequest(r)
	if err != nil {
		return err
	}
	started := false
	err = m.ReadFile(r.Context(), fr, func(h guestexec.FileHead) (io.Writer, error) {
		ct := "application/octet-stream"
		if fr.Archive {
			ct = api.ContentTypeTar
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set(api.HeaderFileMode, guestexec.FormatMode(h.Mode))
		if h.Size >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(h.Size, 10))
		}
		w.WriteHeader(http.StatusOK)
		started = true
		return w, nil
	})
	if err != nil && started {
		// Headers are gone: cut the body short so the client sees a
		// truncated transfer instead of a wrong one.
		s.log.Warn("file read failed mid-stream", "vm", fr.VM, "err", err)
		panic(http.ErrAbortHandler)
	}
	return err
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) error {
	m, err := s.processes()
	if err != nil {
		return err
	}
	fr, err := fileRequest(r)
	if err != nil {
		return err
	}
	var mode uint64
	if v := r.URL.Query().Get("mode"); v != "" {
		if mode, err = strconv.ParseUint(v, 8, 32); err != nil || mode > 0o7777 {
			return domain.Errorf(domain.CodeInvalidArgument, "mode must be octal permission bits like 0644").WithDetail("field", "mode")
		}
	}
	info, err := m.WriteFile(r.Context(), fr, uint32(mode), r.Body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, info)
}
