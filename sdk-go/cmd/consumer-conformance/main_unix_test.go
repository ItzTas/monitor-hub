//go:build unix

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const aliveEnv = "CONSUMER_CONFORMANCE_ALIVE"

type stubborn struct {
	pid       int
	tokenFile string
}

func init() {
	fakes["tool"] = main
	fakes["stubborn"] = fakeStubborn
	fakes["asleep"] = func() { time.Sleep(time.Hour) }
}

func fakeStubborn() {
	signal.Ignore(syscall.SIGTERM)
	pokes := make(chan os.Signal, 1)
	signal.Notify(pokes, syscall.SIGUSR1)
	alive, err := os.OpenFile(os.Getenv(aliveEnv), os.O_WRONLY, 0)
	if err != nil {
		os.Exit(4)
	}
	sleeper := exec.CommandContext(context.Background(), os.Args[0])
	sleeper.Env = append(os.Environ(), modeEnv+"=asleep")
	sleeper.ExtraFiles = []*os.File{alive}
	if err := sleeper.Start(); err != nil {
		os.Exit(4)
	}
	fmt.Printf("ready %d %s\n", os.Getpid(), argument("--token-file="))
	for range pokes {
		fmt.Println("still here")
	}
}

func signalTool(sig syscall.Signal) func(*exec.Cmd, stubborn, io.ReadCloser) {
	return func(tool *exec.Cmd, _ stubborn, output io.ReadCloser) {
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			_, _ = io.Copy(io.Discard, output)
		}()
		_ = tool.Process.Signal(sig)
		<-drained
	}
}

func breakPipe(_ *exec.Cmd, consumer stubborn, output io.ReadCloser) {
	_ = output.Close()
	_ = syscall.Kill(consumer.pid, syscall.SIGUSR1)
}

func TestAStoppedRunLeavesNoConsumerAndNoTokenFile(t *testing.T) {
	t.Parallel()
	stops := map[string]func(*exec.Cmd, stubborn, io.ReadCloser){
		"hangup":      signalTool(syscall.SIGHUP),
		"interrupt":   signalTool(syscall.SIGINT),
		"termination": signalTool(syscall.SIGTERM),
		"broken pipe": breakPipe,
	}
	for name, stop := range stops {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			alivePath := filepath.Join(t.TempDir(), "alive")
			if err := syscall.Mkfifo(alivePath, 0o600); err != nil {
				t.Fatal(err)
			}
			alive, err := os.OpenFile(alivePath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = alive.Close() }()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			tool := exec.CommandContext(ctx, os.Args[0], "-env", modeEnv+"=stubborn", "-env", aliveEnv+"="+alivePath, "--", os.Args[0], "--token-file={token_file}")
			tool.Env = append(os.Environ(), modeEnv+"=tool")
			tool.WaitDelay = time.Second
			output, err := tool.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := tool.Start(); err != nil {
				t.Fatal(err)
			}
			consumer := awaitReady(t, bufio.NewScanner(output))
			t.Cleanup(func() {
				_ = syscall.Kill(-consumer.pid, syscall.SIGKILL)
				_ = os.RemoveAll(filepath.Dir(consumer.tokenFile))
			})
			stop(tool, consumer, output)
			_ = tool.Wait()
			if ctx.Err() != nil {
				t.Fatal("the tool did not stop within 30s")
			}
			if err := alive.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadAll(alive); err != nil {
				t.Errorf("the consumer or its child is still running: %v", err)
			}
			if _, err := os.Stat(consumer.tokenFile); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("token file after the run: err = %v, want it removed", err)
			}
		})
	}
}

func awaitReady(t *testing.T, lines *bufio.Scanner) stubborn {
	t.Helper()
	var seen []string
	for lines.Scan() {
		seen = append(seen, lines.Text())
		ready, ok := strings.CutPrefix(lines.Text(), consumerPrefix+"ready ")
		if !ok {
			continue
		}
		pid, tokenFile, _ := strings.Cut(ready, " ")
		n, err := strconv.Atoi(pid)
		if err != nil {
			t.Fatal(err)
		}
		return stubborn{pid: n, tokenFile: tokenFile}
	}
	t.Fatalf("the consumer never got ready:\n%s", strings.Join(seen, "\n"))
	return stubborn{}
}
