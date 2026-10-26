package cli

import (
	"context"
	"flag"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
)

const resizeUsage = "[ID] [--type T] [--cpus N] [--memory SIZE] [--disk SIZE] [--stop] [--no-wait]"

func resizeFlags(fs *flag.FlagSet, o *opts) {
	shapeFlags(fs, o, true)
	fs.BoolVar(&o.stopFirst, "stop", false, "stop a running VM first (commits its disk), then resize")
	waitFlags(fs, o)
}

// runResize changes a VM's shape at resume (feature resize): a stopped VM
// is resumed with the new shape; a running one is stopped first with
// --stop, else refused. The disk can only grow.
func runResize(ctx context.Context, s *session, o *opts, args []string) error {
	id, err := s.currentID(args)
	if err == nil {
		err = maxArgs(args, 1, "resize "+resizeUsage)
	}
	if err != nil {
		return err
	}
	body := api.ResumeVMRequest{Type: o.typ, Shape: o.shape()}
	if body.Type == "" && body.Shape == nil {
		return usageErr("resize needs a new shape: --type, --cpus, --memory or --disk")
	}
	var vm api.VM
	if err := s.cl.get(ctx, vmPath(id), nil, &vm); err != nil {
		return err
	}
	if vm.State == domain.StateReady || vm.State == domain.StateRunning {
		if !o.stopFirst {
			return clientErr(domain.CodeStopRequired, "%s is %s: a VM is resized when it boots. Pass --stop to stop it (commits the disk) and resume it resized, or run `%s stop %s` first",
				vm.Name, vm.State, buildinfo.Name, vm.Name)
		}
		if err := s.stopForResize(ctx, o, string(vm.ID)); err != nil {
			return err
		}
	}
	return s.lifecycle(ctx, o, "POST", vmPath(string(vm.ID))+"/resume", body)
}

// stopForResize stops id and waits for it whatever --no-wait says (the
// resume needs a stopped VM); only the resume reports a result.
func (s *session) stopForResize(ctx context.Context, o *opts, id string) error {
	key := newIdempotencyKey()
	if o.key != "" {
		key = o.key + "-stop"
	}
	acc, err := s.cl.mutate(ctx, "POST", vmPath(id)+"/stop", api.StopVMRequest{}, key)
	if err != nil {
		return err
	}
	op := acc.Operation
	s.out.emit("operation", op.ID, 0, acc, nil)
	s.out.diag("stop %s before resize: operation %s", op.VMID, op.ID)
	budget := s.waitBudget(o)
	wctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var after int64
	for !op.State.Terminal() {
		page, err := s.cl.events(wctx, op.ID, after, max(min(time.Until(deadline(wctx)), 25*time.Second), 0))
		if err != nil {
			if wctx.Err() != nil {
				e := clientErr(domain.CodeTimeout, "stopped waiting for the stop after %s; operation %s continues, resize not requested", budget, op.ID)
				e.OperationID = op.ID
				return e
			}
			return err
		}
		for _, ev := range page.Items {
			s.out.emit("progress", op.ID, ev.Seq, ev, nil)
			if ev.Phase != "" && ev.Phase != "done" {
				s.out.diag("  %-18s %3.0f%% %s", ev.Phase, ev.Progress*100, ev.Message)
			}
			after = ev.Seq
		}
		op = page.Operation
	}
	if op.State == domain.OpFailed {
		e := &cliError{ErrorBody{Code: domain.CodeInternal, Message: "stop before resize failed", Origin: "operation", OperationID: op.ID}}
		if op.Error != nil {
			e.Code, e.Message, e.Retryable, e.Details = op.Error.Code, "stop before resize failed: "+op.Error.Message, op.Error.Retryable, op.Error.Details
		}
		return e
	}
	return nil
}
