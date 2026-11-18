package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ItzTas/monitor-hub/sdk-go/consumertest"
)

const (
	consumerPrefix = "consumer: "
	lineLimit      = 64 << 10
	exitOK         = 0
	exitViolations = 1
	exitUsage      = 2
	waitDelay      = time.Second
	usage          = "usage: consumer-conformance [-start-within D] [-ack-within D] [-reconnect-within D] [-quiet-after D] [-env NAME=value]... -- <command> [args...]"
)

type envFlags []string

type prefixed struct {
	out  io.Writer
	line []byte
}

func (p *prefixed) Write(b []byte) (int, error) {
	p.line = append(p.line, b...)
	for {
		line, rest, ok := nextLine(p.line)
		if !ok {
			return len(b), nil
		}
		p.line = rest
		if err := p.writeLine(line); err != nil {
			return len(b), err
		}
	}
}

func (p *prefixed) flush() error {
	if len(p.line) == 0 {
		return nil
	}
	line := p.line
	p.line = nil
	return p.writeLine(line)
}

func (p *prefixed) writeLine(line []byte) error {
	_, err := p.out.Write(slices.Concat([]byte(consumerPrefix), line, []byte("\n")))
	return err
}

func nextLine(pending []byte) ([]byte, []byte, bool) {
	if end := bytes.IndexByte(pending[:min(len(pending), lineLimit+1)], '\n'); end >= 0 {
		return pending[:end], pending[end+1:], true
	}
	if len(pending) > lineLimit {
		return pending[:lineLimit], pending[lineLimit:], true
	}
	return nil, pending, false
}

func (e *envFlags) String() string { return strings.Join(*e, ",") }

func (e *envFlags) Set(v string) error {
	if name, _, ok := strings.Cut(v, "="); !ok || name == "" {
		return fmt.Errorf("%q is not NAME=value", v)
	}
	*e = append(*e, v)
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var env envFlags
	opts := consumertest.DefaultOptions()
	fs := flag.NewFlagSet("consumer-conformance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprintln(stderr, usage) }
	fs.DurationVar(&opts.StartWithin, "start-within", opts.StartWithin, "how long the consumer has to open the stream")
	fs.DurationVar(&opts.AckWithin, "ack-within", opts.AckWithin, "how long the consumer has to acknowledge")
	fs.DurationVar(&opts.ReconnectWithin, "reconnect-within", opts.ReconnectWithin, "how long the consumer has to reconnect")
	fs.DurationVar(&opts.QuietAfter, "quiet-after", opts.QuietAfter, "how long to watch the consumer after a 401 or replaced")
	fs.Var(&env, "env", "environment variable NAME=value for the consumer (repeatable)")

	sep := slices.Index(args, "--")
	if sep < 0 || sep == len(args)-1 {
		_, _ = fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	if err := fs.Parse(args[:sep]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return exitUsage
	}

	output := &prefixed{out: stderr}
	violations, err := consumertest.Check(ctx, launcher(ctx, args[sep+1:], env, output), opts)
	_ = output.flush()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "consumer-conformance: %v\n", oneLine(err))
		return exitViolations
	}
	for _, line := range violations {
		_, _ = fmt.Fprintln(stderr, line)
	}
	if len(violations) > 0 {
		return exitViolations
	}
	_, _ = fmt.Fprintln(stdout, "consumer conformance passed")
	return exitOK
}

func launcher(ctx context.Context, command, env []string, output *prefixed) consumertest.Launch {
	return func(url, tokenFile, token string) (func() error, error) {
		replacer := strings.NewReplacer("{url}", url, "{token_file}", tokenFile, "{token}", token)
		cmd := exec.CommandContext(context.WithoutCancel(ctx), command[0], command[1:]...)
		for i := 1; i < len(cmd.Args); i++ {
			cmd.Args[i] = replacer.Replace(cmd.Args[i])
		}
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdout, cmd.Stderr = output, output
		cmd.WaitDelay = waitDelay
		configure(cmd)
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		return func() error {
			terminate(cmd, done)
			return output.flush()
		}, nil
	}
}

func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}
