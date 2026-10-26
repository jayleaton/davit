package httpapi

import (
	"io"
	"net/http"
	"strconv"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/snapfiles"
)

// SetSnapshotFiles enables the snapshot file routes (G12).
func (s *Server) SetSnapshotFiles(f *snapfiles.Service) { s.snapFiles = f }

func (s *Server) snapshotFiles(operation string) (*snapfiles.Service, error) {
	if s.snapFiles == nil {
		return nil, domain.NotImplemented(domain.FeatureSnapshotFiles, operation)
	}
	return s.snapFiles, nil
}

func (s *Server) listSnapshotTree(w http.ResponseWriter, r *http.Request) error {
	f, err := s.snapshotFiles("listSnapshotTree")
	if err != nil {
		return err
	}
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 {
			return domain.Errorf(domain.CodeInvalidArgument, "limit must be a positive integer").WithDetail("field", "limit")
		}
	}
	tree, err := f.Tree(r.Context(), principal(r), r.PathValue("snapshotId"), q.Get("path"), limit, q.Get("cursor"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, tree)
}

func (s *Server) readSnapshotFile(w http.ResponseWriter, r *http.Request) error {
	f, err := s.snapshotFiles("readSnapshotFile")
	if err != nil {
		return err
	}
	q := r.URL.Query()
	archive := false
	switch q.Get("archive") {
	case "":
	case api.ArchiveTar:
		archive = true
	default:
		return domain.Errorf(domain.CodeInvalidArgument, "archive must be %q", api.ArchiveTar).WithDetail("field", "archive")
	}
	return s.streamSnapshot(w, r, func(start snapfiles.Start) (string, error) {
		return f.ReadFile(r.Context(), principal(r), r.PathValue("snapshotId"), q.Get("path"), archive, start)
	})
}

func (s *Server) exportSnapshot(w http.ResponseWriter, r *http.Request) error {
	f, err := s.snapshotFiles("exportSnapshot")
	if err != nil {
		return err
	}
	return s.streamSnapshot(w, r, func(start snapfiles.Start) (string, error) {
		return f.Export(r.Context(), principal(r), r.PathValue("snapshotId"), r.URL.Query().Get("format"), start)
	})
}

// streamSnapshot answers a snapshot read. A digest known up front (a file)
// is a header with Content-Length; a stream's digest is a trailer, which
// needs chunked encoding, so streams carry no Content-Length.
func (s *Server) streamSnapshot(w http.ResponseWriter, r *http.Request, read func(snapfiles.Start) (string, error)) error {
	started, trailer := false, false
	sum, err := read(func(h snapfiles.Head) (io.Writer, error) {
		w.Header().Set("Content-Type", h.ContentType)
		if h.Mode != "" {
			w.Header().Set(api.HeaderFileMode, h.Mode)
		}
		if h.SHA256 != "" {
			w.Header().Set(api.HeaderContentSHA256, h.SHA256)
			w.Header().Set("Content-Length", strconv.FormatInt(h.Size, 10))
		} else {
			w.Header().Set("Trailer", api.HeaderContentSHA256)
			trailer = true
		}
		w.WriteHeader(http.StatusOK)
		started = true
		return w, nil
	})
	if err != nil && started {
		// Headers are gone: cut the body short so the client sees a
		// truncated transfer instead of a wrong one.
		s.log.Warn("snapshot read failed mid-stream", "snapshot", r.PathValue("snapshotId"), "err", err)
		panic(http.ErrAbortHandler)
	}
	if err == nil && trailer {
		w.Header().Set(api.HeaderContentSHA256, sum)
	}
	return err
}
