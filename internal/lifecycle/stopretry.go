package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"davit/internal/domain"
	"davit/internal/runtime"
	"davit/internal/state"
)

// Stop retry policy (G9). A graceful stop asks the guest to shut down up to
// Config.StopAttempts times, StopGrace each. Every attempt the guest lets
// time out is journaled as an operation event, so a stop replayed after a
// daemon restart continues the count instead of starting over. After the
// last attempt the VMM is killed (unless Config.NoStopForce) and the
// snapshot is recorded crash-consistent, as for `stop --force`.

const (
	// DefaultStopAttempts is the graceful stop attempts when none are set.
	DefaultStopAttempts = 3
	// MaxStopAttempts bounds Config.StopAttempts.
	MaxStopAttempts = 10

	stopPhase         = "guest.shutdown"
	stopAttemptPrefix = "attempt "
	stopForcedPrefix  = "stop forced after "
)

// gracefulStop runs the retry policy against h. It reports whether the VMM
// was killed (the disk is then crash-consistent).
func (s *Service) gracefulStop(ctx context.Context, op *domain.Operation, h runtime.Handle) (forced bool, err error) {
	n := s.cfg.StopAttempts
	done, forced, err := s.stopHistory(ctx, op.ID)
	if err != nil {
		return false, err
	}
	var last error
	for i := done + 1; !forced && i <= n; i++ {
		err := s.cfg.Runtime.Stop(ctx, h, s.cfg.StopGrace)
		if err == nil {
			return false, nil
		}
		if ctx.Err() != nil || !(domain.IsCode(err, domain.CodeTimeout) || errors.Is(err, context.DeadlineExceeded)) {
			return false, err // not a guest that ignored the request
		}
		last = err
		msg := fmt.Sprintf("%s%d/%d: guest did not shut down within %s", stopAttemptPrefix, i, n, s.cfg.StopGrace)
		if err := s.journalEvent(ctx, op, msg); err != nil {
			return false, err
		}
	}
	if !forced && s.cfg.NoStopForce {
		if last == nil { // the attempts ran before a restart
			e := domain.Errorf(domain.CodeTimeout, "guest did not shut down after %d attempts", n)
			e.Retryable = true
			last = e
		}
		return false, last
	}
	if !forced {
		// Journaled before the kill: a replay must not record a clean snapshot.
		if err := s.journalEvent(ctx, op, fmt.Sprintf("%s%d graceful attempts: VMM killed, disk is crash-consistent", stopForcedPrefix, max(n, done))); err != nil {
			return false, err
		}
	}
	return true, s.cfg.Runtime.Kill(ctx, h)
}

// stopHistory counts op's journaled failed attempts and whether it already
// decided to force.
func (s *Service) stopHistory(ctx context.Context, id domain.OperationID) (attempts int, forced bool, err error) {
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		var after int64
		for {
			evs, err := tx.Events(id, after, 500)
			if err != nil {
				return err
			}
			if len(evs) == 0 {
				return nil
			}
			for _, ev := range evs {
				after = ev.Seq
				if ev.Phase != stopPhase {
					continue
				}
				switch {
				case strings.HasPrefix(ev.Message, stopAttemptPrefix):
					attempts++
				case strings.HasPrefix(ev.Message, stopForcedPrefix):
					forced = true
				}
			}
		}
	})
	return attempts, forced, err
}

// journalEvent appends an event in phase guest.shutdown and fails if it
// could not be recorded (unlike event, which is informational).
func (s *Service) journalEvent(ctx context.Context, op *domain.Operation, msg string) error {
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		return tx.AppendEvent(&domain.OperationEvent{OperationID: op.ID, Time: s.now(), State: op.State, Phase: stopPhase, Progress: op.Progress, Message: msg})
	})
	if err == nil {
		s.notify()
	}
	return err
}
