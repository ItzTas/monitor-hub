package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	modeEnv = "CONSUMER_CONFORMANCE_FAKE"
	outEnv  = "CONSUMER_CONFORMANCE_OUT"
)

type recorded struct {
	Args      []string `json:"args"`
	TokenFile string   `json:"token_file"`
	Mode      uint32   `json:"mode"`
	Content   string   `json:"content"`
}

func TestMain(m *testing.M) {
	switch os.Getenv(modeEnv) {
	case "":
		os.Exit(m.Run())
	case "record":
		fakeRecord()
	case "boom":
		_, _ = fmt.Fprintln(os.Stderr, "boom")
		os.Exit(3)
	case "good":
		fakeGood()
	}
	os.Exit(0)
}

func fakeRecord() {
	rec := recorded{Args: os.Args[1:]}
	for _, arg := range rec.Args {
		if file, ok := strings.CutPrefix(arg, "--token-file="); ok {
			rec.TokenFile = file
		}
	}
	info, err := os.Stat(rec.TokenFile)
	if err != nil {
		os.Exit(4)
	}
	content, err := os.ReadFile(rec.TokenFile)
	if err != nil {
		os.Exit(4)
	}
	rec.Mode, rec.Content = uint32(info.Mode().Perm()), string(content)
	data, _ := json.Marshal(rec)
	if err := os.WriteFile(os.Getenv(outEnv), data, 0o600); err != nil {
		os.Exit(4)
	}
	select {}
}

func fakeGood() {
	hub, tokenFile := strings.TrimPrefix(os.Args[1], "--hub="), os.Args[2]
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		os.Exit(4)
	}
	token := strings.TrimSpace(string(raw))
	for {
		wait := time.Second
		switch status := fakeSession(hub, token); status {
		case http.StatusUnauthorized:
			wait = time.Minute
		case -1:
			select {}
		}
		time.Sleep(wait)
	}
}

func fakeSession(hub, token string) int {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, hub+"/consumer/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode
	}
	event, id := "", ""
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case line == "":
			if event == "replaced" {
				return -1
			}
			if (event == "notification" || event == "caught_up") && id != "" {
				fakeAck(hub, token, id)
			}
			event, id = "", ""
		}
	}
	return 0
}

func fakeAck(hub, token, id string) {
	seq, _ := strconv.ParseInt(id, 10, 64)
	body, _ := json.Marshal(map[string]int64{"seq": seq})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, hub+"/consumer/v1/ack", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func runConsumer(t *testing.T, mode string, flags []string, args ...string) (int, string, string) {
	t.Helper()
	out := t.TempDir() + "/recorded.json"
	command := append([]string{"-env", modeEnv + "=" + mode, "-env", outEnv + "=" + out}, flags...)
	command = append(command, "--", os.Args[0])
	command = append(command, args...)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), command, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func readRecorded(t *testing.T, stderrOut string, path string) recorded {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the consumer recorded nothing: %v\n%s", err, stderrOut)
	}
	var rec recorded
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func recordRun(t *testing.T, args ...string) recorded {
	t.Helper()
	out := t.TempDir() + "/recorded.json"
	command := []string{"-start-within", "300ms", "-env", modeEnv + "=record", "-env", outEnv + "=" + out, "--", os.Args[0]}
	var stdout, stderr bytes.Buffer
	run(context.Background(), append(command, args...), &stdout, &stderr)
	return readRecorded(t, stderr.String(), out)
}

func TestUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"no separator":      {"-start-within", "1s"},
		"no command":        {"-start-within", "1s", "--"},
		"env without equal": {"-env", "SEMIGUAL", "--", "consumer"},
		"stray argument":    {"stray", "--", "consumer"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), args, &stdout, &stderr)
			if code != exitUsage {
				t.Fatalf("code = %d, want %d", code, exitUsage)
			}
			if !strings.Contains(stderr.String(), usage) {
				t.Fatalf("stderr = %q, want the usage line", stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing", stdout.String())
			}
		})
	}
}

func TestPlaceholdersAreReplacedInsideArguments(t *testing.T) {
	rec := recordRun(t, "--hub={url}", "--token-file={token_file}", "--token={token}", "plain")
	if len(rec.Args) != 4 {
		t.Fatalf("args = %q, want 4 arguments", rec.Args)
	}
	if !strings.HasPrefix(rec.Args[0], "--hub=http://127.0.0.1:") || strings.ContainsAny(rec.Args[0], "{}") {
		t.Fatalf("args[0] = %q, want --hub=http://127.0.0.1:<port>", rec.Args[0])
	}
	if rec.TokenFile == "" || strings.ContainsAny(rec.TokenFile, "{}") {
		t.Fatalf("token file = %q, want a path", rec.TokenFile)
	}
	if want := strings.TrimPrefix(rec.Args[2], "--token=") + "\n"; rec.Content != want {
		t.Fatalf("token file content = %q, want %q, the token of args[2] and a newline", rec.Content, want)
	}
	if !strings.HasPrefix(rec.Content, "mhc_") {
		t.Fatalf("token file content = %q, want a token", rec.Content)
	}
	if rec.Args[3] != "plain" {
		t.Fatalf("args[3] = %q, want plain", rec.Args[3])
	}
}

func TestTheTokenFileIsPrivateAndRemoved(t *testing.T) {
	rec := recordRun(t, "--token-file={token_file}")
	if rec.Mode != 0o600 {
		t.Fatalf("token file mode = %o, want 600", rec.Mode)
	}
	if _, err := os.Stat(rec.TokenFile); !os.IsNotExist(err) {
		t.Fatalf("token file after the run: err = %v, want it removed", err)
	}
}

func TestAConsumerThatExitsAtOnceIsReported(t *testing.T) {
	code, stdout, stderr := runConsumer(t, "boom", []string{"-start-within", "300ms"})
	if code != exitViolations {
		t.Fatalf("code = %d, want %d\n%s", code, exitViolations, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
	boom := strings.Index(stderr, "consumer: boom\n")
	violation := strings.Index(stderr, "the consumer did not open the stream within")
	if boom < 0 || violation < 0 || boom > violation {
		t.Fatalf("stderr = %q, want consumer: boom before the violation", stderr)
	}
}

func TestAWellBehavedConsumerPassesEndToEnd(t *testing.T) {
	flags := []string{"-ack-within", "2s", "-reconnect-within", "5s", "-quiet-after", "500ms"}
	code, stdout, stderr := runConsumer(t, "good", flags, "--hub={url}", "{token_file}")
	if code != exitOK {
		t.Fatalf("code = %d, want %d\n%s", code, exitOK, stderr)
	}
	if stdout != "consumer conformance passed\n" {
		t.Fatalf("stdout = %q, want the pass line", stdout)
	}
}
