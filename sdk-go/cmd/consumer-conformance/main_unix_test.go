//go:build unix

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
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
	fakes["crash"] = fakeCrash
	fakes["announced"] = func() {
		fmt.Println("ready")
		fakeGood()
	}
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

// fakeCrash reads the stream a byte at a time and exits on the unknown event
// with the rest of the frame unread, so the connection is reset, as when a
// consumer crashes.
func fakeCrash() {
	hub := strings.TrimPrefix(argument("--hub="), "http://")
	token, err := os.ReadFile(argument("--token-file="))
	if err != nil {
		os.Exit(4)
	}
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", hub)
	if err != nil {
		os.Exit(4)
	}
	_, _ = fmt.Fprintf(conn, "GET /consumer/v1/stream HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n", hub, strings.TrimSpace(string(token)))
	seen := ""
	for b := make([]byte, 1); !strings.HasSuffix(seen, "event: future_thing\n"); seen += string(b) {
		if _, err := conn.Read(b); err != nil {
			os.Exit(4)
		}
	}
	_, _ = fmt.Fprintln(os.Stderr, "crashing on an unknown event")
	os.Exit(3)
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
	stops := map[string]struct {
		signal syscall.Signal
		stop   func(*exec.Cmd, stubborn, io.ReadCloser)
	}{
		"hangup":      {syscall.SIGHUP, signalTool(syscall.SIGHUP)},
		"interrupt":   {syscall.SIGINT, signalTool(syscall.SIGINT)},
		"termination": {syscall.SIGTERM, signalTool(syscall.SIGTERM)},
		"broken pipe": {stop: breakPipe},
	}
	for name, tt := range stops {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if tt.signal != 0 && signal.Ignored(tt.signal) {
				t.Skipf("the tests run with %v ignored, which the tool inherits and keeps", tt.signal)
			}
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
			tt.stop(tool, consumer, output)
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

func TestAConsumerThatCrashesMidStreamKeepsItsViolations(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	tool := exec.CommandContext(ctx, os.Args[0], "-ack-within", "500ms", "-reconnect-within", "1s", "-quiet-after", "100ms", "-env", modeEnv+"=crash", "--", os.Args[0], "--hub={url}", "--token-file={token_file}")
	tool.Env = append(os.Environ(), modeEnv+"=tool")
	var stdout, stderr bytes.Buffer
	tool.Stdout, tool.Stderr = &stdout, &stderr
	var exit *exec.ExitError
	if err := tool.Run(); !errors.As(err, &exit) || exit.ExitCode() != exitViolations {
		t.Fatalf("tool: %v, want exit status %d\n%s", err, exitViolations, stderr.String())
	}
	want := strings.Join([]string{
		"consumer: crashing on an unknown event",
		"no ack reached seq 3 within 500ms of caught_up",
		"no ack reached seq 4 within 500ms after an unknown event type",
		"the consumer did not reconnect within 1s after the stream ended",
	}, "\n") + "\n"
	if stderr.String() != want || stdout.Len() != 0 {
		t.Fatalf("stdout = %q, stderr:\n%s\nwant no stdout and stderr:\n%s", stdout.String(), stderr.String(), want)
	}
}

func TestASignalIgnoredAtStartStaysIgnored(t *testing.T) {
	t.Parallel()
	for name, sig := range map[string]syscall.Signal{"HUP": syscall.SIGHUP, "INT": syscall.SIGINT} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			ignoring := "trap '' " + name + `; exec "$0" "$@"`
			args := slices.Concat([]string{"-c", ignoring, os.Args[0]}, quickFlags(), []string{"-env", modeEnv + "=announced", "--", os.Args[0], "--hub={url}", "--token-file={token_file}"})
			tool := exec.CommandContext(ctx, "sh", args...)
			tool.Env = append(os.Environ(), modeEnv+"=tool")
			var stdout bytes.Buffer
			tool.Stdout = &stdout
			output, err := tool.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := tool.Start(); err != nil {
				t.Fatal(err)
			}
			lines := bufio.NewScanner(output)
			for lines.Scan() {
				if lines.Text() == consumerPrefix+"ready" {
					break
				}
			}
			_ = tool.Process.Signal(sig)
			var after []string
			for lines.Scan() {
				after = append(after, lines.Text())
			}
			if err := tool.Wait(); err != nil || stdout.String() != "consumer conformance passed\n" {
				t.Fatalf("tool: %v, stdout %q, stderr after the signal:\n%s\nwant the run to pass", err, stdout.String(), strings.Join(after, "\n"))
			}
		})
	}
}
