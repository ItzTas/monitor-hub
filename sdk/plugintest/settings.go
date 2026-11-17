package plugintest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	settingsFile  = "settings.json"
	validateArg   = "validate-settings"
	validateLimit = 5 * time.Second
	answerLimit   = 64 << 10
)

const (
	supportedSettingsVersion = 1
	validatorWaitDelay       = time.Second
	answerSpace              = " \t\n\f\r"
)

var byteOrderMark = []byte{0xEF, 0xBB, 0xBF}

type settingsDocument struct {
	SettingsVersion *int64           `json:"settings_version"`
	Fields          *[]settingsField `json:"fields"`
}

type settingsField struct {
	Name     *string         `json:"name"`
	Label    *string         `json:"label"`
	Help     *string         `json:"help"`
	Type     *string         `json:"type"`
	Secret   json.RawMessage `json:"secret"`
	Required json.RawMessage `json:"required"`
	Default  *string         `json:"default"`
	Options  *[]string       `json:"options"`
	Group    *string         `json:"group"`
	Editable json.RawMessage `json:"editable"`
	Reason   *string         `json:"reason"`
}

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

func CheckSettings(ctx context.Context, dir, program string, stderr io.Writer) []string {
	if violation := dirViolation(dir); violation != "" {
		return []string{violation}
	}
	data, err := os.ReadFile(filepath.Join(dir, settingsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	out := fileViolations(data, err)
	if violation := validatorViolation(ctx, dir, program, stderr); violation != "" {
		out = append(out, violation)
	}
	return out
}

func dirViolation(dir string) string {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Sprintf("%s: cannot read: %v", settingsFile, err)
	}
	if !info.IsDir() {
		return fmt.Sprintf("%s: cannot read: %s is not a directory", settingsFile, dir)
	}
	return ""
}

func fileViolations(data []byte, err error) []string {
	if err != nil {
		return []string{fmt.Sprintf("%s: cannot read: %v", settingsFile, err)}
	}
	return settingsViolations(data)
}

func validatorViolation(ctx context.Context, dir, program string, stderr io.Writer) string {
	state, stdout, violation := runValidator(ctx, dir, program, stderr)
	if violation != "" {
		return violation
	}
	return answerViolation(state, stdout)
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

func answerViolation(state *os.ProcessState, stdout []byte) string {
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
	listed, violation := readAnswer(stdout)
	if violation != "" {
		return violation
	}
	if code == 0 && listed {
		return validateArg + " exited with code 0 but listed problems"
	}
	if code == 1 && !listed {
		return validateArg + " exited with code 1 but listed no problems"
	}
	return ""
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

func readAnswer(stdout []byte) (bool, string) {
	notObject := validateArg + " did not print a single JSON object on stdout: "
	invalid := validateArg + " answer is invalid: "
	dec := json.NewDecoder(bytes.NewReader(stdout))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return false, notObject + err.Error()
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return false, notObject + "trailing data after the JSON object"
	}
	if raw[0] != '{' {
		return false, notObject + "the answer is not an object"
	}
	var answer validatorAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return false, invalid + err.Error()
	}
	if answer.Problems == nil {
		return false, invalid + "problems is missing"
	}
	problems := *answer.Problems
	if i := slices.IndexFunc(problems, func(p validatorProblem) bool { return p.Message == nil }); i >= 0 {
		return false, invalid + fmt.Sprintf("problem %d has no message", i)
	}
	if i := slices.IndexFunc(problems, func(p validatorProblem) bool { return *p.Message == "" }); i >= 0 {
		return false, invalid + fmt.Sprintf("problem %d has an empty message", i)
	}
	return len(problems) > 0, ""
}

func notValid(detail string) []string {
	return []string{settingsFile + " is not valid: " + detail}
}

func settingsViolations(data []byte) []string {
	if bytes.HasPrefix(data, byteOrderMark) {
		return []string{settingsFile + " starts with a UTF-8 byte order mark"}
	}
	if !utf8.Valid(data) {
		return notValid("the file is not UTF-8")
	}
	doc, problem := decodeSettings(data)
	if problem != "" {
		return notValid(problem)
	}
	if doc.SettingsVersion == nil {
		return []string{settingsFile + ": settings_version is missing"}
	}
	if *doc.SettingsVersion != supportedSettingsVersion {
		return []string{fmt.Sprintf("%s: settings_version is %d, want %d", settingsFile, *doc.SettingsVersion, supportedSettingsVersion)}
	}
	if doc.Fields == nil {
		return []string{settingsFile + ": fields is missing"}
	}
	var out []string
	seen := map[string]bool{}
	for i := range *doc.Fields {
		out = append(out, fieldViolations(i, &(*doc.Fields)[i], seen)...)
	}
	return out
}

func decodeSettings(data []byte) (settingsDocument, string) {
	var doc settingsDocument
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return doc, err.Error()
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return doc, "trailing data after the JSON object"
	}
	if problem := unknownKey(data); problem != "" {
		return doc, problem
	}
	if doc.Fields == nil {
		return doc, ""
	}
	return doc, badFlag(*doc.Fields)
}

func unknownKey(data []byte) string {
	topKeys := map[string]bool{"settings_version": true, "fields": true}
	fieldKeys := map[string]bool{
		"name": true, "label": true, "help": true, "type": true, "secret": true, "required": true,
		"default": true, "options": true, "group": true, "editable": true, "reason": true,
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return err.Error()
	}
	if key := firstUnknown(top, topKeys); key != "" {
		return fmt.Sprintf("json: unknown field %q", key)
	}
	var fields []map[string]json.RawMessage
	if err := json.Unmarshal(top["fields"], &fields); err != nil {
		return ""
	}
	for _, field := range fields {
		if key := firstUnknown(field, fieldKeys); key != "" {
			return fmt.Sprintf("json: unknown field %q", key)
		}
	}
	return ""
}

func firstUnknown(got map[string]json.RawMessage, known map[string]bool) string {
	for _, key := range slices.Sorted(maps.Keys(got)) {
		if !known[key] {
			return key
		}
	}
	return ""
}

func badFlag(fields []settingsField) string {
	for _, f := range fields {
		flags := []struct {
			name string
			raw  json.RawMessage
		}{{"secret", f.Secret}, {"required", f.Required}, {"editable", f.Editable}}
		for _, flag := range flags {
			if _, ok := readFlag(flag.raw, false); !ok {
				return flag.name + " must be true or false"
			}
		}
	}
	return ""
}

func readFlag(raw json.RawMessage, absent bool) (value, ok bool) {
	if raw == nil {
		return absent, true
	}
	var v *bool
	if json.Unmarshal(raw, &v) != nil || v == nil {
		return false, false
	}
	return *v, true
}

func fieldViolations(index int, f *settingsField, seen map[string]bool) []string {
	prefix := fmt.Sprintf("%s: fields[%d]: ", settingsFile, index)
	var missing []string
	for _, required := range []struct {
		name string
		set  bool
	}{{"name", f.Name != nil}, {"label", f.Label != nil}, {"type", f.Type != nil}} {
		if !required.set {
			missing = append(missing, prefix+required.name+" is missing")
		}
	}
	if len(missing) > 0 {
		return missing
	}
	if !slices.Contains(fieldTypes, *f.Type) {
		return []string{prefix + fmt.Sprintf("type %q is not one of %s", *f.Type, strings.Join(fieldTypes, ", "))}
	}
	rules := []func(*settingsField, map[string]bool) string{
		checkName, checkLabel, checkOptions, checkSecret, checkReason, checkGroup,
	}
	for _, rule := range rules {
		if problem := rule(f, seen); problem != "" {
			return []string{prefix + problem}
		}
	}
	return nil
}

var fieldTypes = []string{"string", "list", "integer", "number", "duration", "choice"}

func checkName(f *settingsField, seen map[string]bool) string {
	name := *f.Name
	if !validSettingName(name) {
		return fmt.Sprintf("name %q does not match ^[A-Z][A-Z0-9_]*$", name)
	}
	if reservedSettingName(name) {
		return fmt.Sprintf("name %q is reserved by the hub", name)
	}
	if seen[name] {
		return fmt.Sprintf("name %q is declared more than once", name)
	}
	seen[name] = true
	return ""
}

func validSettingName(name string) bool {
	if name == "" || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func reservedSettingName(name string) bool {
	if name == "PATH" || name == "TZ" || name == "TMPDIR" {
		return true
	}
	for _, prefix := range []string{"MONITOR_HUB_", "PLUGIN_", "LD_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func checkLabel(f *settingsField, _ map[string]bool) string {
	if strings.TrimSpace(*f.Label) == "" {
		return "label is empty"
	}
	return ""
}

func checkOptions(f *settingsField, _ map[string]bool) string {
	if *f.Type != "choice" {
		if f.Options != nil {
			return "options are only for choice fields"
		}
		return ""
	}
	if f.Options == nil || len(*f.Options) == 0 {
		return "a choice field needs at least one option"
	}
	seen := map[string]bool{}
	for _, option := range *f.Options {
		if strings.TrimSpace(option) == "" {
			return "an option is empty"
		}
		if seen[option] {
			return fmt.Sprintf("option %q is listed more than once", option)
		}
		seen[option] = true
	}
	if f.Default != nil && !seen[*f.Default] {
		return fmt.Sprintf("default %q is not one of the options", *f.Default)
	}
	return ""
}

func checkSecret(f *settingsField, _ map[string]bool) string {
	if secret, _ := readFlag(f.Secret, false); secret && f.Default != nil {
		return "a secret field cannot have a default"
	}
	return ""
}

func checkReason(f *settingsField, _ map[string]bool) string {
	editable, _ := readFlag(f.Editable, true)
	if editable {
		if f.Reason != nil {
			return "reason is only for fields with editable: false"
		}
		return ""
	}
	if f.Reason == nil || strings.TrimSpace(*f.Reason) == "" {
		return "editable: false needs a non-empty reason"
	}
	return ""
}

func checkGroup(f *settingsField, _ map[string]bool) string {
	if f.Group != nil && strings.TrimSpace(*f.Group) == "" {
		return "group is empty"
	}
	return ""
}
