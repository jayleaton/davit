package cli

import (
	"context"
	"flag"

	"davit/api"
)

// recoverFlags are `recover`'s flags: confirmation and the usual waiting.
func recoverFlags(fs *flag.FlagSet, o *opts) {
	fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	waitFlags(fs, o)
}

// runRecover power-cycles a wedged VM on its current disk (G9): the guest
// is killed without a commit and its disk boots as is.
func runRecover(ctx context.Context, s *session, o *opts, args []string) error {
	id, err := s.currentID(args)
	if err == nil {
		err = maxArgs(args, 1, "recover [ID] [--yes]")
	}
	if err != nil {
		return err
	}
	if err := s.confirm(o, "Power-cycle "+id+"? Its processes and memory are lost; the disk boots as it is (like a power loss)."); err != nil {
		return err
	}
	return s.lifecycle(ctx, o, "POST", vmPath(id)+"/recover", api.RecoverVMRequest{})
}
