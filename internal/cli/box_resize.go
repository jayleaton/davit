package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
)

// Box resize: --type/--cpus/--memory/--disk on box up/run/ssh/sync (and
// image build/update) also apply to a box that already exists. A VM's
// shape changes only when it boots, and a resume always cold boots the
// last committed disk (golden RAM restores are for creates, keyed by
// shape), so:
//
//   - a stopped box resumes with the new shape;
//   - a running one is stopped (its disk committed) and resumed with it,
//     with a note on stderr, unless processes started through davit still
//     run in it: those would die with the VM, so it refuses and says what
//     to run.
//
// The host decides whether the shape changes: a resume of a running VM is
// a no-op when it does not and stop_required when it does, answered at
// once even while others queue for capacity, and a shape the host refuses
// (above its limits, a smaller disk) fails before anything is stopped. The
// resume after the stop is admitted like any other, through the capacity
// queue (--wait), and the host keeps the new shape for every later resume.

// resizeStep is what stopToResize did.
type resizeStep int

const (
	resizeNone    resizeStep = iota // the shape is unchanged
	resizeStopped                   // stopped: resume it with the new shape
	resizeResumed                   // it had stopped meanwhile; the probe resumed it with the new shape
	resizeAgain                     // an operation started meanwhile: look again
)

// stopToResize stops running vm when o's shape flags change its shape and
// nothing started through davit runs in it.
func (s *session) stopToResize(ctx context.Context, o *opts, vm api.VM) (resizeStep, error) {
	probe := api.ResumeVMRequest{Type: o.typ, Shape: o.shape()}
	acc, err := s.capacityMutate(ctx, vmPath(string(vm.ID))+"/resume", probe, o.boxWait())
	switch {
	case err == nil && acc.Operation.Result != nil && acc.Operation.Result.NoOp:
		return resizeNone, nil
	case err == nil:
		return resizeResumed, nil
	case isCode(err, domain.CodeOperationInProgress):
		return resizeAgain, nil
	case !isCode(err, domain.CodeStopRequired):
		return resizeNone, err
	}
	box := boxRefOf(vm)
	live, err := s.liveProcesses(ctx, vm)
	if err != nil {
		return resizeNone, err
	}
	if len(live) > 0 {
		ids := live
		if len(ids) > 3 {
			ids = append(ids[:3:3], "...")
		}
		down, up := boxCommands(vm)
		e := clientErr(domain.CodeStopRequired, "box %s runs with %s; the new shape needs a restart, which would end the %d process(es) "+
			"started through %s still running in it (%s). When they are done, run `%s` and then `%s%s`",
			box, shapeLine(vm.Shape), len(live), buildinfo.Name, strings.Join(ids, ", "), down, up, shapeArgs(o))
		e.Details = map[string]any{"reason": "processes_running", "processes": live}
		return resizeNone, e
	}
	s.out.diag("box %s: resizing from %s to%s: stopping it (disk committed; ssh sessions end) and starting it again", box, shapeLine(vm.Shape), shapeArgs(o))
	return resizeStopped, s.stopBox(ctx, o, string(vm.ID))
}

// liveProcesses lists the processes started through the API that still
// run in vm's current boot.
func (s *session) liveProcesses(ctx context.Context, vm api.VM) ([]string, error) {
	var ids []string
	for cursor := ""; ; {
		q := url.Values{"limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var l api.ProcessList
		if err := s.cl.get(ctx, vmPath(string(vm.ID))+"/processes", q, &l); err != nil {
			return nil, err
		}
		for _, p := range l.Items {
			if p.Generation == vm.InstanceGeneration && (p.Status == api.ProcessRunning || p.Status == api.ProcessStarting) {
				ids = append(ids, p.ID)
			}
		}
		if cursor = l.NextCursor; cursor == "" {
			return ids, nil
		}
	}
}

// resizeStranded adds to the failed resume of a box stopped for a resize
// that it stays stopped, its disk kept, and how to bring it back.
func resizeStranded(err error, vm api.VM) error {
	var ce *cliError
	if !errors.As(err, &ce) {
		return err
	}
	_, up := boxCommands(vm)
	ce.Message += fmt.Sprintf("; box %s was stopped for the resize and stays stopped, its disk kept: `%s` without --type/--cpus/--memory/--disk "+
		"starts it with its old shape (%s)", boxRefOf(vm), up, shapeLine(vm.Shape))
	return err
}

// boxCommands are the commands that stop vm and bring it up again: box
// down and box up for an instance, stop and box image update for a
// project's builder (box up does not take it).
func boxCommands(vm api.VM) (down, up string) {
	if vm.Labels[labelBoxRole] == boxRoleImage {
		return fmt.Sprintf("%s stop %s", buildinfo.Name, vm.Name), fmt.Sprintf("%s box image update %s", buildinfo.Name, vm.Labels[domain.LabelBox])
	}
	return fmt.Sprintf("%s box down %s", buildinfo.Name, boxRefOf(vm)), fmt.Sprintf("%s box up %s", buildinfo.Name, boxRefOf(vm))
}

// boxRefOf names vm as box commands take it: PROJECT/INSTANCE.
func boxRefOf(vm api.VM) string {
	inst := vm.Labels[labelBoxInstance]
	if vm.Labels[labelBoxRole] == boxRoleImage {
		inst = imageInstance
	}
	if inst == "" {
		return vm.Name
	}
	return vm.Labels[domain.LabelBox] + "/" + inst
}

// shapeLine is a shape for messages: "2 cpu, 2.0 GiB memory, 20.0 GiB disk".
func shapeLine(sh domain.Shape) string {
	return fmt.Sprintf("%d cpu, %s memory, %s disk", sh.CPUs, humanBytes(sh.MemoryBytes), humanBytes(sh.DiskBytes))
}

// shapeArgs is o's shape flags as typed again (" --memory 4GiB").
func shapeArgs(o *opts) string {
	var b strings.Builder
	if o.typ != "" {
		b.WriteString(" --type " + o.typ)
	}
	if o.cpus != 0 {
		b.WriteString(" --cpus " + strconv.Itoa(o.cpus))
	}
	if o.memory.v != 0 {
		b.WriteString(" --memory " + sizeArg(o.memory.v))
	}
	if o.disk.v != 0 {
		b.WriteString(" --disk " + sizeArg(o.disk.v))
	}
	return b.String()
}

// sizeArg writes n bytes as a --memory/--disk value, in the largest exact
// unit.
func sizeArg(n int64) string {
	for _, u := range []struct {
		suffix string
		mul    int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if n%u.mul == 0 {
			return strconv.FormatInt(n/u.mul, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}
