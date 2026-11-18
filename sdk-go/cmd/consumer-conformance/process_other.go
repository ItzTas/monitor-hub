//go:build !unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

var stopSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}

func configure(*exec.Cmd) {}

func terminate(cmd *exec.Cmd, done <-chan struct{}) {
	_ = cmd.Process.Kill()
	<-done
}
