package cli

import (
	"context"
	"fmt"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
)

// batchOutcome is one item of `new --count`: the server's answer and, after
// waiting, the operation's final state and the VM.
type batchOutcome struct {
	api.BatchItem
	Final *api.Operation `json:"final,omitempty"`
	VM    *api.VM        `json:"vm,omitempty"`
}

// newBatch is `new --count N`: one createVMs request (feature batchCreate),
// one line per item, then (unless --no-wait) every accepted operation is
// followed to its end. Any refused or failed item makes the exit nonzero,
// with the first failure's code.
func (s *session) newBatch(ctx context.Context, o *opts, tmpl api.CreateVMRequest) error {
	if o.count < 1 || o.count > api.MaxBatchCount {
		return usageErr("--count must be 1..%d", api.MaxBatchCount)
	}
	var res api.CreateVMsResult
	req := api.CreateVMsRequest{Count: o.count, Template: tmpl}
	if err := s.cl.do(ctx, "POST", "/vms/batch", nil, req, map[string]string{api.HeaderIdempotencyKey: o.idempotencyKey()}, &res); err != nil {
		return err
	}
	out := make([]batchOutcome, len(res.Items))
	var first *cliError
	failed := 0
	fail := func(e ErrorBody) {
		failed++
		if first == nil {
			first = &cliError{e}
		}
	}
	for i, it := range res.Items {
		out[i].BatchItem = it
		s.out.emit("item", opID(it), 0, it, nil)
		if it.Error != nil {
			s.out.human("%3d %-24s refused: %s (%s)", it.Index, orDash(it.Name), it.Error.Message, it.Error.Code)
			fail(ErrorBody{Code: it.Error.Code, Message: it.Error.Message, Retryable: it.Error.Retryable,
				Details: it.Error.Details, Origin: "server", RequestID: it.Error.RequestID})
			continue
		}
		s.out.human("%3d %-24s %s accepted: operation %s", it.Index, it.Name, it.Accepted.Operation.VMID, it.Accepted.Operation.ID)
	}
	if !o.noWait {
		budget := s.waitBudget(o)
		wctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		for i := range out {
			acc := out[i].Accepted
			if acc == nil {
				continue
			}
			op, err := s.waitQuiet(wctx, acc.Operation.ID)
			if err != nil {
				if wctx.Err() != nil {
					return clientErr(domain.CodeTimeout, "stopped waiting after %s; the operations continue (query: %s operations ID --watch)", budget, buildinfo.Name)
				}
				return err
			}
			out[i].Final = op
			if op.State == domain.OpFailed {
				e := ErrorBody{Code: domain.CodeInternal, Message: "operation failed", Origin: "operation", OperationID: op.ID}
				if op.Error != nil {
					e.Code, e.Message, e.Retryable, e.Details = op.Error.Code, op.Error.Message, op.Error.Retryable, op.Error.Details
				}
				fail(e)
				s.out.human("%3d %-24s failed: %s (%s)", out[i].Index, out[i].Name, e.Message, e.Code)
				continue
			}
			var vm api.VM
			if err := s.cl.get(ctx, vmPath(string(op.VMID)), nil, &vm); err == nil {
				out[i].VM = &vm
				s.out.human("%3d %-24s %s (%s)", out[i].Index, vm.Name, vm.State, vm.BootMode)
			}
		}
	}
	s.out.result("", map[string]any{"items": out, "failed": failed})
	if first != nil {
		first.Message = fmt.Sprintf("%d of %d items failed; first: %s", failed, len(out), first.Message)
		return first
	}
	return nil
}

func opID(it api.BatchItem) domain.OperationID {
	if it.Accepted != nil {
		return it.Accepted.Operation.ID
	}
	return ""
}

// waitQuiet long-polls op until it is terminal, without progress output.
func (s *session) waitQuiet(ctx context.Context, id domain.OperationID) (*api.Operation, error) {
	var after int64
	for {
		wait := min(time.Until(deadline(ctx)), 25*time.Second)
		page, err := s.cl.events(ctx, id, after, max(wait, 0))
		if err != nil {
			return nil, err
		}
		for _, ev := range page.Items {
			s.out.emit("progress", id, ev.Seq, ev, nil)
			after = ev.Seq
		}
		if page.Operation.State.Terminal() && len(page.Items) == 0 {
			op := page.Operation
			return &op, nil
		}
	}
}
