//go:build !unix

package main

import "os/exec"

func configure(*exec.Cmd) {}

func terminate(cmd *exec.Cmd, done <-chan struct{}) {
	_ = cmd.Process.Kill()
	<-done
}
