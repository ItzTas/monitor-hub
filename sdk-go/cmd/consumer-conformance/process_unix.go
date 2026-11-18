//go:build unix

package main

import (
	"os/exec"
	"syscall"
	"time"
)

const termGrace = 2 * time.Second

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
