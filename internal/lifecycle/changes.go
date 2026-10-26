package lifecycle

// Changed returns a channel that is closed at the next journal write (any VM
// or operation change). Call it again after each wake; read state after
// taking the channel so no change is missed.
func (s *Service) Changed() <-chan struct{} { return s.watch() }
