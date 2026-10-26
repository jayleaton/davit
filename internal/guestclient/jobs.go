package guestclient

import (
	"context"

	"davit/internal/guestproto"
)

// Lookup finds a managed job by the Exec.Handle it was started with (feature
// jobs). A handle no process carries this boot is a CodeNotFound error.
func (c *Client) Lookup(ctx context.Context, handle string) (guestproto.ProcessStatus, error) {
	if err := c.require(guestproto.FeatureJobs); err != nil {
		return guestproto.ProcessStatus{}, err
	}
	req := guestproto.ProcessLookup{Handle: handle}
	if err := req.Validate(); err != nil {
		return guestproto.ProcessStatus{}, err
	}
	return roundTrip[guestproto.ProcessStatus](ctx, c, req)
}

// ProcessList samples every guest process (feature ps).
func (c *Client) ProcessList(ctx context.Context) (guestproto.ProcessList, error) {
	if err := c.require(guestproto.FeaturePS); err != nil {
		return guestproto.ProcessList{}, err
	}
	return roundTrip[guestproto.ProcessList](ctx, c, guestproto.ProcessListRequest{})
}
