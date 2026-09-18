package server

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// clientWatchInterval is how often watchClientProcess polls whether
// initialize's processId is still alive.
const clientWatchInterval = 10 * time.Second

// processAlive reports whether the OS process pid still exists. Indirected
// through a var, like lifecycle.go's graphLoad, so tests can substitute a
// fake without spawning and killing a real process for every case.
//
// Dead is reported ONLY for os.ErrProcessDone or syscall.ESRCH; every other
// outcome (permission denied signaling a process owned by another user, an
// unsupported platform) is treated as alive. golance would rather run
// forever as an orphan on a platform or permission edge case it cannot
// positively confirm than shut itself down while its client is actually
// still running.
var processAlive = func(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH)
}

// watchClientProcess polls whether pid — the parent process that started
// this session, per initialize's processId — is still alive, calling
// s.opts.ClientGone (if set) at most once and returning as soon as it finds
// pid gone. Returns without ever calling ClientGone if ctx is done first
// (the session ended on its own, e.g. a normal "exit").
//
// This exists because a dead editor does not guarantee stdin's write end
// ever closes: a surviving child process that inherited the pipe's write
// end keeps it open, so Serve's read loop blocks on readFrame forever
// instead of observing EOF, leaving this session running as an orphan that
// keeps holding the shared facts index's exclusive lock (see
// internal/store's package doc) for every other session on this root. The
// LSP spec requires exactly this behavior for initialize's processId.
func (s *Server) watchClientProcess(ctx context.Context, pid int) {
	ticker := time.NewTicker(clientWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !processAlive(pid) {
			s.logger.Printf("golance: client process %d is gone; shutting down", pid)
			if s.opts.ClientGone != nil {
				s.opts.ClientGone()
			}
			return
		}
	}
}
