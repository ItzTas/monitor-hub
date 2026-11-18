//go:build unix

package plugintest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	validatorWaitDelay = time.Second
	answerSpace        = " \t\n\f\r"
)

type validatorAnswer struct {
	Problems *[]validatorProblem `json:"problems"`
}

type validatorProblem struct {
	Name    string  `json:"name"`
	Message *string `json:"message"`
}

type cappedBuffer struct {
	data  []byte
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p[:min(b.limit-len(b.data), len(p))]...)
	return len(p), nil
}

func validatorViolation(ctx context.Context, dir, program string, declared map[string]bool, stderr io.Writer) string {
	state, stdout, violation := runValidator(ctx, dir, program, stderr)
	if violation != "" {
		return violation
	}
	return answerViolation(state, stdout, declared)
}

func runValidator(ctx context.Context, dir, program string, stderr io.Writer) (*os.ProcessState, []byte, string) {
	program, err := filepath.Abs(program)
	if err != nil {
		return nil, nil, validateArg + " could not be started: " + err.Error()
	}
	limit, cancel := context.WithTimeout(ctx, validateLimit)
	defer cancel()
	var stopped atomic.Bool
	stdout := &cappedBuffer{limit: answerLimit + 1}
	cmd := exec.CommandContext(limit, program, validateArg)
	cmd.Env = []string{}
	cmd.Dir = dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		stopped.Store(true)
		return killGroup(cmd)
	}
	cmd.WaitDelay = validatorWaitDelay
	if err := cmd.Start(); err != nil {
		return nil, nil, validateArg + " could not be started: " + err.Error()
	}
	err = cmd.Wait()
	_ = killGroup(cmd)
	if stopped.Load() {
		return nil, nil, stopViolation(ctx)
	}
	if cmd.ProcessState == nil {
		return nil, nil, validateArg + " could not be waited for: " + err.Error()
	}
	return cmd.ProcessState, stdout.data, ""
}

func killGroup(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func stopViolation(ctx context.Context) string {
	if err := ctx.Err(); err != nil {
		return validateArg + " was interrupted: " + err.Error()
	}
	return fmt.Sprintf("%s did not finish within %s", validateArg, validateLimit)
}

func answerViolation(state *os.ProcessState, stdout []byte, declared map[string]bool) string {
	code, violation := exitViolation(state)
	if violation != "" {
		return violation
	}
	if len(stdout) > answerLimit {
		return fmt.Sprintf("%s wrote more than %d bytes on stdout", validateArg, answerLimit)
	}
	if len(bytes.Trim(stdout, answerSpace)) == 0 {
		return validateArg + " printed nothing on stdout; it runs with an empty environment and without MONITOR_HUB_PLUGIN"
	}
	problems, violation := readAnswer(stdout)
	if violation != "" {
		return violation
	}
	if code == 0 && len(problems) > 0 {
		return validateArg + " exited with code 0 but listed problems"
	}
	if code == 1 && len(problems) == 0 {
		return validateArg + " exited with code 1 but listed no problems"
	}
	return undeclaredViolation(problems, declared)
}

func undeclaredViolation(problems []validatorProblem, declared map[string]bool) string {
	if declared == nil {
		return ""
	}
	i := slices.IndexFunc(problems, func(p validatorProblem) bool { return p.Name != "" && !declared[p.Name] })
	if i < 0 {
		return ""
	}
	return fmt.Sprintf("%s answer is invalid: problem %d names %q, which %s does not declare", validateArg, i, problems[i].Name, settingsFile)
}

func exitViolation(state *os.ProcessState) (int, string) {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 0, fmt.Sprintf("%s was killed by signal %d", validateArg, int(status.Signal()))
	}
	code := state.ExitCode()
	if code != 0 && code != 1 {
		return code, fmt.Sprintf("%s exited with code %d, want 0 or 1", validateArg, code)
	}
	return code, ""
}

func readAnswer(stdout []byte) ([]validatorProblem, string) {
	notObject := validateArg + " did not print a single JSON object on stdout: "
	invalid := validateArg + " answer is invalid: "
	dec := json.NewDecoder(bytes.NewReader(stdout))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, notObject + err.Error()
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, notObject + "trailing data after the JSON object"
	}
	if raw[0] != '{' {
		return nil, notObject + "the answer is not an object"
	}
	var answer validatorAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, invalid + err.Error()
	}
	if answer.Problems == nil {
		return nil, invalid + "problems is missing"
	}
	problems := *answer.Problems
	if i := slices.IndexFunc(problems, func(p validatorProblem) bool { return p.Message == nil }); i >= 0 {
		return nil, invalid + fmt.Sprintf("problem %d has no message", i)
	}
	if i := slices.IndexFunc(problems, func(p validatorProblem) bool { return *p.Message == "" }); i >= 0 {
		return nil, invalid + fmt.Sprintf("problem %d has an empty message", i)
	}
	return problems, ""
}
