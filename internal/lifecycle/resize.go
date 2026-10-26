package lifecycle

import (
	"davit/api"
	"davit/internal/domain"
)

// resizeShape resolves the shape a resume boots with (feature resize, G9):
// shape override fields over the profile body.Type names over the VM's
// current shape, the same resolution create and fork use. CPUs and memory
// change freely within the runtime's constraints; the disk only grows. A
// profile with a smaller disk keeps the current one (as fork does); an
// explicit smaller shape.diskBytes is refused (reason disk_shrink). Without
// type or shape it returns the current shape.
func (s *Service) resizeShape(cur domain.Shape, body api.ResumeVMRequest) (domain.Shape, error) {
	if body.Type == "" && body.Shape == nil {
		return cur, nil
	}
	base, err := s.profileShape(body.Type, cur)
	if err != nil {
		return cur, err
	}
	base.DiskBytes = max(base.DiskBytes, cur.DiskBytes)
	if o := body.Shape; o != nil && o.DiskBytes > 0 && o.DiskBytes < cur.DiskBytes {
		return cur, domain.Errorf(domain.CodeInvalidArgument, "disk cannot shrink: %d bytes requested, the VM has %d", o.DiskBytes, cur.DiskBytes).
			WithDetail("field", "shape.diskBytes").WithDetail("currentBytes", cur.DiskBytes).WithDetail("reason", "disk_shrink")
	}
	return s.resolveShape(body.Shape, base, cur.DiskBytes)
}

// resizeAdmission is what a resume reserves: the CPUs and memory of the
// shape it boots (a stopped VM holds none) plus the disk growth (the
// current disk is already reserved).
func resizeAdmission(cur, next domain.Shape) domain.Shape {
	return domain.Shape{CPUs: next.CPUs, MemoryBytes: next.MemoryBytes, DiskBytes: max(next.DiskBytes-cur.DiskBytes, 0)}
}

// resizeRefusal reports a resize asked of a VM that is not stopped: a
// running VM keeps its shape until it is stopped and resumed.
func resizeRefusal(vm *domain.VM, next domain.Shape) error {
	if next == vm.Shape {
		return nil
	}
	return domain.Errorf(domain.CodeStopRequired, "vm %s is %s: stop it before resizing", vm.ID, vm.State).
		WithDetail("state", vm.State)
}
