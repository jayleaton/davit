// Package jobs serves durable inspection of managed processes (G7): job
// history across generations, host-retained output (readable while the VM
// is stopped and after a daemon restart) and whole-guest process samples.
// guestexec records output and samples; this package reads them. Owner:
// CLI/API.
package jobs

import (
	"context"
	"encoding/base64"
	"strconv"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/state"
)

// VMs resolves a VM visible to a principal in any state (lifecycle.Service).
type VMs interface {
	GetVM(ctx context.Context, principal domain.PrincipalID, ref string) (*domain.VM, error)
}

// Sampler samples a running VM's guest processes (guestexec.Manager's
// GuestProcesses, guestproto feature "ps"): it maps managed jobs' handles to
// process IDs and records the sample for later stale reads.
type Sampler interface {
	GuestProcesses(ctx context.Context, principal domain.PrincipalID, ref string) (api.GuestProcessList, error)
}

// Config configures a Service.
type Config struct {
	DB  *state.DB
	VMs VMs
	// Sampler enables listGuestProcesses on running VMs; nil means not
	// implemented (stopped VMs still return their last sample).
	Sampler Sampler
	Now     func() time.Time
}

// Service is safe for concurrent use.
type Service struct{ cfg Config }

// New returns a Service.
func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg}
}

// MaxOutputPage bounds chunks per readProcessOutput page.
const MaxOutputPage = 200

func processAPI(r *state.ProcessRecord) api.Process {
	argv := r.Argv
	if argv == nil {
		argv = []string{}
	}
	return api.Process{ID: r.ID, VMID: r.VMID, Generation: r.Generation, Status: api.ProcessStatus(r.Status), Argv: argv,
		Detached: r.Detached, ExitCode: r.ExitCode, Signal: r.Signal, TimedOut: r.TimedOut,
		OutputCursor: r.OutputCursor, CreatedAt: r.CreatedAt, ExitedAt: r.ExitedAt}
}

// History returns one page of ref's managed jobs, newest first.
func (s *Service) History(ctx context.Context, principal domain.PrincipalID, ref string, limit int, cursor string) (api.HistoryList, error) {
	vm, err := s.cfg.VMs.GetVM(ctx, principal, ref)
	if err != nil {
		return api.HistoryList{}, err
	}
	if limit <= 0 || limit > api.MaxListLimit {
		limit = api.DefaultListLimit
	}
	out := api.HistoryList{Items: []api.HistoryEntry{}}
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		recs, next, err := tx.ListProcessHistory(principal, vm.ID, state.Page{Limit: limit, Cursor: cursor, Filter: "history|" + string(vm.ID)})
		if err != nil {
			return err
		}
		out.NextCursor = next
		for _, r := range recs {
			st, err := tx.OutputStats(r.ID)
			if err != nil {
				return err
			}
			e := api.HistoryEntry{Process: processAPI(r), Handle: r.Handle, StdoutBytes: st.Stdout, StderrBytes: st.Stderr, OutputDropped: st.Dropped}
			if st.Withheld {
				e.OutputNotRetained = api.OutputNotRetainedPolicy
			}
			out.Items = append(out.Items, e)
		}
		return nil
	})
	return out, err
}

// Output returns retained output of process id after cursor (a seq), for
// stream "stdout", "stderr" or "both" ("" = both).
func (s *Service) Output(ctx context.Context, principal domain.PrincipalID, ref, id, cursor, stream string, limit int) (api.ProcessOutput, error) {
	var after int64
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 0 {
			return api.ProcessOutput{}, domain.Errorf(domain.CodeInvalidArgument, "invalid cursor").WithDetail("field", "after")
		}
		after = n
	}
	switch stream {
	case "", "both", api.StreamStdout, api.StreamStderr:
	default:
		return api.ProcessOutput{}, domain.Errorf(domain.CodeInvalidArgument, "stream must be stdout, stderr or both").WithDetail("field", "stream")
	}
	if limit <= 0 || limit > MaxOutputPage {
		limit = MaxOutputPage
	}
	vm, err := s.cfg.VMs.GetVM(ctx, principal, ref)
	if err != nil {
		return api.ProcessOutput{}, err
	}
	out := api.ProcessOutput{Items: []api.ProcessStreamMessage{}}
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		rec, err := tx.GetProcess(id)
		if err != nil || rec.Owner != principal || rec.VMID != vm.ID {
			return domain.Errorf(domain.CodeNotFound, "process %s not found", id)
		}
		// Read whole pages and filter, so the cursor stays a seq.
		chunks, err := tx.ReadOutput(id, after, limit)
		if err != nil {
			return err
		}
		for _, c := range chunks {
			after = c.Seq
			if stream == "" || stream == "both" || stream == c.Stream {
				out.Items = append(out.Items, api.ProcessStreamMessage{Type: c.Stream, Seq: c.Seq, Data: base64.StdEncoding.EncodeToString(c.Data)})
			}
		}
		if len(chunks) > 0 {
			out.NextCursor = strconv.FormatInt(after, 10)
		}
		ended := rec.Status != string(api.ProcessStarting) && rec.Status != string(api.ProcessRunning) && rec.Status != string(api.ProcessUnknown)
		out.Complete = ended && len(chunks) < limit
		return nil
	})
	return out, err
}

// PS samples ref's guest processes. A running VM is sampled live and the
// sample kept; a VM that is not running returns its last sample (stale).
func (s *Service) PS(ctx context.Context, principal domain.PrincipalID, ref string) (api.GuestProcessList, error) {
	vm, err := s.cfg.VMs.GetVM(ctx, principal, ref)
	if err != nil {
		return api.GuestProcessList{}, err
	}
	if vm.State != domain.StateReady && vm.State != domain.StateRunning {
		var last *api.GuestProcessList
		err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
			last, err = tx.GuestSample(vm.ID)
			return err
		})
		if err != nil {
			return api.GuestProcessList{}, err
		}
		last.Stale = true
		return *last, nil
	}
	if s.cfg.Sampler == nil {
		return api.GuestProcessList{}, domain.NotImplemented(domain.FeatureGuestPS, "listGuestProcesses")
	}
	return s.cfg.Sampler.GuestProcesses(ctx, principal, ref)
}
