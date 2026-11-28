package monitorconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/catalog"
	"github.com/ItzTas/monitor-hub/src/internal/memfile"
)

// ValidatorError says why a monitor's validate-settings gave no usable answer. Stderr is the end
// of what it wrote there, which may hold secrets, so Error never includes it.
type ValidatorError struct {
	Reason string
	Stderr string
}

func (e *ValidatorError) Error() string {
	return "the monitor could not validate the values: " + e.Reason
}

type Validator struct {
	Limit       time.Duration
	WaitDelay   time.Duration
	AnswerLimit int
	StderrLimit int
}

func NewValidator() *Validator {
	return &Validator{Limit: 5 * time.Second, WaitDelay: time.Second, AnswerLimit: 65536, StderrLimit: 4096}
}

// cappedBuffer keeps the first limit bytes and drops the rest without failing the writer.
type cappedBuffer struct {
	data  []byte
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p[:min(b.limit-len(b.data), len(p))]...)
	return len(p), nil
}

// tailBuffer keeps the last limit bytes.
type tailBuffer struct {
	data  []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if extra := len(b.data) - b.limit; extra > 0 {
		b.data = slices.Clone(b.data[extra:])
	}
	return len(p), nil
}

// Validate runs the monitor's validate-settings with document on descriptor 3 and env as its
// whole environment. entry.Settings must not be nil.
func (v *Validator) Validate(ctx context.Context, entry catalog.Entry, env []string, document []byte) ([]Problem, error) {
	path, err := catalog.ResolveCommand(entry, env)
	if err != nil {
		return nil, &ValidatorError{Reason: "its program could not be found: " + err.Error()}
	}
	limit, cancel := context.WithTimeout(ctx, v.Limit)
	defer cancel()
	var stopped atomic.Bool
	stdout := &cappedBuffer{limit: v.AnswerLimit + 1}
	stderr := &tailBuffer{limit: v.StderrLimit}
	cmd := exec.CommandContext(limit, path, settings.ValidateArg)
	cmd.Dir = entry.Dir
	cmd.Env = append(slices.Clip(env), settings.ConfigFDEnv+"=3")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		stopped.Store(true)
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = v.WaitDelay
	if err := start(cmd, document); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &ValidatorError{Reason: "it could not be started: " + err.Error()}
	}
	_ = cmd.Wait()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if stopped.Load() {
		return nil, &ValidatorError{Reason: fmt.Sprintf("it did not finish within %s", v.Limit), Stderr: string(stderr.data)}
	}
	problems, reason := v.answer(cmd.ProcessState, stdout.data, entry.Settings)
	if reason != "" {
		return nil, &ValidatorError{Reason: reason, Stderr: string(stderr.data)}
	}
	return problems, nil
}

// start hands document over on a memfile that the hub closes as soon as the start ends.
func start(cmd *exec.Cmd, document []byte) error {
	f, err := memfile.New("monitor-hub-config", document)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	cmd.ExtraFiles = []*os.File{f}
	return cmd.Start()
}

func (v *Validator) answer(state *os.ProcessState, stdout []byte, decl *settings.Declaration) ([]Problem, string) {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return nil, fmt.Sprintf("it was killed by signal %d", int(status.Signal()))
	}
	code := state.ExitCode()
	if code != 0 && code != 1 {
		return nil, fmt.Sprintf("it exited with code %d", code)
	}
	if len(stdout) > v.AnswerLimit {
		return nil, fmt.Sprintf("it wrote more than %d bytes on stdout", v.AnswerLimit)
	}
	if !utf8.Valid(stdout) {
		return nil, "its answer is not UTF-8"
	}
	problems, reason := readProblems(stdout, decl)
	if reason != "" {
		return nil, reason
	}
	if code == 0 && len(problems) > 0 {
		return nil, "it exited with code 0 but listed problems"
	}
	if code == 1 && len(problems) == 0 {
		return nil, "it exited with code 1 but listed no problems"
	}
	return problems, ""
}

func readProblems(stdout []byte, decl *settings.Declaration) ([]Problem, string) {
	dec := json.NewDecoder(bytes.NewReader(stdout))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil || raw[0] != '{' {
		return nil, "its answer is not a single JSON object"
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, "its answer is not a single JSON object"
	}
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, "its answer is not a single JSON object"
	}
	var objects []map[string]json.RawMessage
	if err := json.Unmarshal(answer["problems"], &objects); err != nil || objects == nil || slices.ContainsFunc(objects, isNil) {
		return nil, "its answer has no problems list"
	}
	problems := make([]Problem, len(objects))
	for i, object := range objects {
		if err := json.Unmarshal(object["message"], &problems[i].Message); err != nil || strings.TrimSpace(problems[i].Message) == "" {
			return nil, fmt.Sprintf("problem %d has no message", i)
		}
	}
	for i, object := range objects {
		if p, ok := object["path"]; ok && json.Unmarshal(p, &problems[i].Path) != nil {
			return nil, fmt.Sprintf("problem %d has a path that is not text", i)
		}
	}
	for i, p := range problems {
		if !decl.Declares(p.Path) {
			return nil, fmt.Sprintf("problem %d names the path %q, which settings.json does not declare", i, p.Path)
		}
	}
	return problems, ""
}

func isNil(object map[string]json.RawMessage) bool {
	return object == nil
}
