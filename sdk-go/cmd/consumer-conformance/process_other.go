//go:build !unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

var stopSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}

// Outside Unix a closed stdout or stderr does not kill the tool, and the run
// goes on to its end.
func catchBrokenPipes() {}

func brokenPipe(error) bool { return false }

func configure(*exec.Cmd) {}

func terminate(cmd *exec.Cmd, done <-chan struct{}) {
	_ = cmd.Process.Kill()
	<-done
}
