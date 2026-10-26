package guestexec

// SetCrash makes Exec stop at stage as if the daemon died there.
func SetCrash(m *Manager, f func(stage string) bool) { m.crash = f }
