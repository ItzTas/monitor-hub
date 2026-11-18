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
	"path/filepath"
	"slices"
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
	Args         []string `json:"args"`
	TokenFile    string   `json:"token_file"`
	Mode         uint32   `json:"mode"`
	Content      string   `json:"content"`
	NoProxy      string   `json:"no_proxy_upper"`
	LowerNoProxy string   `json:"no_proxy_lower"`
}

var fakes = map[string]func(){
	"record": fakeRecord,
	"boom":   fakeBoom,
	"good":   fakeGood,
}

func TestMain(m *testing.M) {
	fake, ok := fakes[os.Getenv(modeEnv)]
	if !ok {
		os.Exit(m.Run())
	}
	fake()
	os.Exit(0)
}

func argument(prefix string) string {
	for _, arg := range os.Args[1:] {
		if value, ok := strings.CutPrefix(arg, prefix); ok {
			return value
		}
	}
	return ""
}

func fakeBoom() {
	_, _ = fmt.Fprintln(os.Stderr, "boom")
	os.Exit(3)
}

func fakeRecord() {
	rec := recorded{
		Args:         os.Args[1:],
		TokenFile:    argument("--token-file="),
		NoProxy:      os.Getenv("NO_PROXY"),
		LowerNoProxy: os.Getenv("no_proxy"),
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
	fakeGood()
}

func fakeGood() {
	hub := argument("--hub=")
	raw, err := os.ReadFile(argument("--token-file="))
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
	command := append([]string{"-env", modeEnv + "=" + mode}, flags...)
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

func quickFlags() []string {
	return []string{"-ack-within", "2s", "-reconnect-within", "5s", "-quiet-after", "500ms"}
}

func recordRun(t *testing.T, flags []string, args ...string) recorded {
	t.Helper()
	out := filepath.Join(t.TempDir(), "recorded.json")
	flags = slices.Concat(quickFlags(), []string{"-env", outEnv + "=" + out}, flags)
	code, _, stderr := runConsumer(t, "record", flags, args...)
	if code != exitOK {
		t.Fatalf("code = %d, want %d\n%s", code, exitOK, stderr)
	}
	return readRecorded(t, stderr, out)
}

func TestUsageErrors(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		args []string
		want string
	}{
		"no separator":              {args: []string{"-start-within", "1s"}, want: usage},
		"no command":                {args: []string{"-start-within", "1s", "--"}, want: usage},
		"env without equal":         {args: []string{"-env", "SEMIGUAL", "--", "consumer"}, want: `invalid value "SEMIGUAL" for flag -env`},
		"stray argument":            {args: []string{"stray", "--", "consumer"}, want: usage},
		"a zero start-within":       {args: []string{"-start-within", "0s", "--", "consumer"}, want: `invalid value "0s" for flag -start-within: must be greater than zero`},
		"a negative ack-within":     {args: []string{"-ack-within", "-1s", "--", "consumer"}, want: `invalid value "-1s" for flag -ack-within: must be greater than zero`},
		"a zero reconnect-within":   {args: []string{"-reconnect-within", "0", "--", "consumer"}, want: `invalid value "0" for flag -reconnect-within: must be greater than zero`},
		"a negative quiet-after":    {args: []string{"-quiet-after", "-5ms", "--", "consumer"}, want: `invalid value "-5ms" for flag -quiet-after: must be greater than zero`},
		"a duration without a unit": {args: []string{"-ack-within", "10", "--", "consumer"}, want: `invalid value "10" for flag -ack-within`},
	}
	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, &stdout, &stderr)
			if code != exitUsage {
				t.Fatalf("code = %d, want %d", code, exitUsage)
			}
			if !strings.Contains(stderr.String(), tt.want) || !strings.Contains(stderr.String(), usage) {
				t.Fatalf("stderr = %q, want %q and the usage line", stderr.String(), tt.want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing", stdout.String())
			}
		})
	}
}

func TestPlaceholdersAreReplacedInsideArguments(t *testing.T) {
	t.Parallel()
	rec := recordRun(t, nil, "--hub={url}", "--token-file={token_file}", "--token={token}", "plain")
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
	t.Parallel()
	rec := recordRun(t, nil, "--hub={url}", "--token-file={token_file}")
	if rec.Mode != 0o600 {
		t.Fatalf("token file mode = %o, want 600", rec.Mode)
	}
	if _, err := os.Stat(rec.TokenFile); !os.IsNotExist(err) {
		t.Fatalf("token file after the run: err = %v, want it removed", err)
	}
}

func TestTheConsumerReachesTheFakeHubWithoutAProxy(t *testing.T) {
	t.Parallel()
	rec := recordRun(t, []string{"-env", "NO_PROXY=corp.example", "-env", "no_proxy=intra.example"}, "--hub={url}", "--token-file={token_file}")
	want := "corp.example,intra.example,127.0.0.1,localhost"
	if rec.NoProxy != want || rec.LowerNoProxy != want {
		t.Errorf("NO_PROXY = %q and no_proxy = %q, want %q in both", rec.NoProxy, rec.LowerNoProxy, want)
	}
}

func TestBothNoProxySpellingsGetOneMergedList(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		env  []string
		want string
	}{
		"neither set":   {want: "127.0.0.1,localhost"},
		"only NO_PROXY": {env: []string{"NO_PROXY=corp.example"}, want: "corp.example,127.0.0.1,localhost"},
		"only no_proxy": {env: []string{"no_proxy=corp.example"}, want: "corp.example,127.0.0.1,localhost"},
		"both, with spaces and repeats": {
			env:  []string{"NO_PROXY=corp.example, intra.example", "no_proxy=intra.example,,localhost"},
			want: "corp.example,intra.example,localhost,127.0.0.1",
		},
		"the last value of a name": {env: []string{"NO_PROXY=old.example", "NO_PROXY=new.example"}, want: "new.example,127.0.0.1,localhost"},
		"a wildcard in NO_PROXY":   {env: []string{"NO_PROXY=*"}, want: "*"},
		"a wildcard in no_proxy":   {env: []string{"NO_PROXY=", "no_proxy= * "}, want: "*"},
		"a wildcard among hosts":   {env: []string{"NO_PROXY=*", "no_proxy=corp.example"}, want: "*,corp.example,127.0.0.1,localhost"},
	}
	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := bypassProxies(tt.env)
			if upper, lower := lookup(env, "NO_PROXY"), lookup(env, "no_proxy"); upper != tt.want || lower != tt.want {
				t.Errorf("NO_PROXY = %q and no_proxy = %q, want %q in both", upper, lower, tt.want)
			}
		})
	}
}

func TestAConsumerThatExitsAtOnceIsReported(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	code, stdout, stderr := runConsumer(t, "good", quickFlags(), "--hub={url}", "--token-file={token_file}")
	if code != exitOK {
		t.Fatalf("code = %d, want %d\n%s", code, exitOK, stderr)
	}
	if stdout != "consumer conformance passed\n" {
		t.Fatalf("stdout = %q, want the pass line", stdout)
	}
}
