//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

const termGrace = 2 * time.Second

var stopSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}

// catchBrokenPipes makes a write to a closed stdout or stderr fail with EPIPE
// instead of killing the tool. The channel is never read: the fake hub's
// writes to a connection the consumer reset raise SIGPIPE too, and those must
// not end the run.
func catchBrokenPipes() {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
}

func brokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE)
}

func configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminate(cmd *exec.Cmd, done <-chan struct{}) {
	pgid := -cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(termGrace):
	}
	_ = syscall.Kill(pgid, syscall.SIGKILL)
	<-done
}
