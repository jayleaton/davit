package guestclient

import (
	"context"

	"davit/internal/guestproto"
)

// PrepareWorkspace runs req (feature workspace) and returns its result,
// calling progress (if non-nil) for each step report. A failed step is a
// WorkspaceResult with State failed, not an error; errors mean the agent
// refused the request or the session failed, so the outcome is unknown.
// Cancelling ctx abandons the stream; closing the session kills a running
// clone or setup step.
func (c *Client) PrepareWorkspace(ctx context.Context, req guestproto.WorkspacePrepare, progress func(guestproto.WorkspaceProgress)) (guestproto.WorkspaceResult, error) {
	var none guestproto.WorkspaceResult
	if err := c.require(guestproto.FeatureWorkspace); err != nil {
		return none, err
	}
	if err := req.Validate(); err != nil {
		return none, err
	}
	cl, err := c.start(req, true)
	if err != nil {
		return none, err
	}
	defer cl.release()
	var seqs guestproto.SeqTracker
	f, err := cl.first(ctx)
	for ; err == nil; f, err = cl.next(ctx) {
		if f.Error != nil {
			return none, f.Error
		}
		if dup, err := seqs.Accept(f); err != nil {
			return none, err
		} else if dup {
			continue
		}
		m, err := guestproto.DecodeMessage(f)
		if err != nil {
			return none, err
		}
		switch m := m.(type) {
		case *guestproto.WorkspaceProgress:
			if progress != nil {
				progress(*m)
			}
		case *guestproto.WorkspaceResult:
			return *m, nil
		default:
			return none, guestproto.Errorf(guestproto.CodeUnexpectedType, "unexpected %q on workspace stream", f.Type)
		}
	}
	return none, err
}
