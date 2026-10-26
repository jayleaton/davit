package userspace

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// controlTimeout bounds a control exchange when ctx has no deadline.
const controlTimeout = 5 * time.Second

// controlRequest is the one message of a control connection.
type controlRequest struct {
	Mode  string   `json:"mode"`
	Allow []string `json:"allow,omitempty"`
}

// controlReply answers it.
type controlReply struct {
	OK    bool   `json:"ok,omitempty"`
	Error string `json:"error,omitempty"`
}

// SetPolicy asks the stack listening on control (Config.Control) to switch
// to mode/allow (domain.EgressMode, normalized domain.Egress.Allow) and
// returns once it is in force: pins and guest flows the new policy denies
// are gone by then.
func SetPolicy(ctx context.Context, control, mode string, allow []string) error {
	if control == "" {
		return errors.New("userspace: no control socket")
	}
	c, err := dialControl(ctx, control)
	if err != nil {
		return err
	}
	defer c.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(controlTimeout)
	}
	_ = c.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if err := json.NewEncoder(c).Encode(controlRequest{Mode: mode, Allow: allow}); err != nil {
		return controlErr(ctx, err)
	}
	var rep controlReply
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&rep); err != nil {
		return controlErr(ctx, err)
	}
	if !rep.OK {
		return errors.New("userspace: set policy: " + rep.Error)
	}
	return nil
}

func controlErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return fmt.Errorf("userspace: set policy: %w", err)
}
