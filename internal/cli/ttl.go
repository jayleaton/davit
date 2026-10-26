package cli

import (
	"context"
	"fmt"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
)

// runExtend restarts a VM's auto-stop countdown: `extend [ID] [DURATION]`.
// With a DURATION it also becomes the VM's TTL; without one the VM's current
// TTL restarts from now. A lone argument that parses as a duration is the
// duration (the VM then comes from DAVIT_CURRENT_ID or the saved context).
func runExtend(ctx context.Context, s *session, o *opts, args []string) error {
	if err := maxArgs(args, 2, "extend [ID] [DURATION]"); err != nil {
		return err
	}
	var ref, dur string
	switch len(args) {
	case 2:
		ref, dur = args[0], args[1]
	case 1:
		if _, err := time.ParseDuration(args[0]); err == nil {
			dur = args[0]
		} else {
			ref = args[0]
		}
	}
	id, err := s.currentID([]string{ref})
	if err != nil {
		return err
	}
	auto := true
	body := api.UpdateVMRequest{AutoStop: &auto}
	if dur != "" {
		d, err := time.ParseDuration(dur)
		if err != nil || d <= 0 {
			return usageErr("DURATION must be positive, like 30m or 2h")
		}
		secs := int64(d / time.Second)
		body.TTLSeconds = &secs
	}
	return s.update(ctx, id, body)
}

// runRename renames a VM: `rename ID NEW-NAME`.
func runRename(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) != 2 {
		return usageErr("usage: %s rename ID NEW-NAME", buildinfo.Name)
	}
	return s.update(ctx, args[0], api.UpdateVMRequest{Name: &args[1]})
}

func (s *session) update(ctx context.Context, ref string, body api.UpdateVMRequest) error {
	var vm api.VM
	if err := s.cl.do(ctx, "PATCH", vmPath(ref), nil, body, nil, &vm); err != nil {
		return err
	}
	s.out.result("", vm)
	printVM(s.out, &vm)
	return nil
}

// autoStopNotice warns on stderr once a VM is inside its auto-stop warning
// window (the host's ttlWarning before expiresAt).
func autoStopNotice(p *printer, vm *api.VM) {
	if vm.AutoStopWarningAt == nil || vm.ExpiresAt == nil || time.Now().Before(*vm.AutoStopWarningAt) {
		return
	}
	if vm.State != domain.StateReady && vm.State != domain.StateRunning {
		return
	}
	left := time.Until(*vm.ExpiresAt).Round(time.Second)
	if left < 0 {
		left = 0
	}
	fmt.Fprintf(p.stderr, "notice: %s auto-stops in %s (at %s); extend it: %s extend %s [DURATION]\n",
		vm.Name, left, vm.ExpiresAt.Local().Format("15:04:05"), buildinfo.Name, vm.Name)
}
