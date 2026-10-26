package cli

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"davit/api"
	"davit/internal/domain"
)

// watchPoll is how often info --watch re-reads a VM with no active
// operation to long-poll.
const watchPoll = time.Second

func infoFlags(fs *flag.FlagSet, o *opts) {
	fs.BoolVar(&o.watch, "watch", false, "follow changes until Ctrl-C (or --until)")
	fs.StringVar(&o.until, "until", "", "with --watch: stop once the VM is in one of these comma-separated states")
	fs.DurationVar(&o.waitTimeout, "wait-timeout", 0, "with --watch: give up after this long (exit 7)")
}

// parseStates parses a comma-separated list of VM states.
func parseStates(v string) ([]domain.VMState, error) {
	var out []domain.VMState
	for _, f := range strings.Split(v, ",") {
		st := domain.VMState(strings.TrimSpace(f))
		if !slices.Contains(domain.AllStates, st) {
			return nil, usageErr("--until: unknown state %q (want one of %v)", st, domain.AllStates)
		}
		out = append(out, st)
	}
	return out, nil
}

// watchVM prints the VM whenever its stateRevision changes: in JSON one
// `item` per change and, once --until is reached, a terminal `result`.
// While an operation is active it long-polls that operation's events;
// otherwise it re-reads the VM every watchPoll. Ctrl-C ends it with exit
// 130; an explicit --wait-timeout or --timeout ends it with exit 7.
func (s *session) watchVM(ctx context.Context, o *opts, ref string) error {
	var until []domain.VMState
	if o.until != "" {
		var err error
		if until, err = parseStates(o.until); err != nil {
			return err
		}
	}
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = sigCtx
	budget := o.waitTimeout
	if budget == 0 {
		budget = s.g.timeout
	}
	if budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	// ctxErr turns a failure caused by the interrupt or the budget into its
	// exit; other errors pass through.
	ctxErr := func(err error) error {
		switch {
		case sigCtx.Err() != nil:
			return &exitStatus{code: ExitInterrupt}
		case ctx.Err() != nil:
			return clientErr(domain.CodeTimeout, "stopped watching after %s", budget)
		}
		return err
	}

	id, rev := ref, int64(-1)
	var op, settled domain.OperationID // op being long-polled; last one seen terminal
	var after int64
	for {
		var vm api.VM
		if err := s.cl.get(ctx, vmPath(id), nil, &vm); err != nil {
			return ctxErr(err)
		}
		id = string(vm.ID) // a deleted VM resolves by ID only
		if vm.StateRevision != rev {
			rev = vm.StateRevision
			s.out.emit("item", "", 0, vm, nil)
			s.out.human("--- %s", time.Now().Format(time.RFC3339))
			printVM(s.out, &vm)
		}
		if slices.Contains(until, vm.State) {
			s.out.result("", vm)
			return nil
		}
		if a := vm.ActiveOperationID; a != "" && a != settled {
			if a != op {
				op, after = a, 0
			}
			page, err := s.cl.events(ctx, op, after, max(min(time.Until(deadline(ctx)), 25*time.Second), 0))
			if err != nil {
				return ctxErr(err)
			}
			if n := len(page.Items); n > 0 {
				after = page.Items[n-1].Seq
			} else if page.Operation.State.Terminal() {
				settled = op // the VM may still name it briefly; poll instead of spinning
			}
			continue
		}
		select {
		case <-time.After(watchPoll):
		case <-ctx.Done():
			return ctxErr(ctx.Err())
		}
	}
}
