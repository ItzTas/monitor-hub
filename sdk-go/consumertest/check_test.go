package consumertest_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/sdk-go/consumertest"
)

const (
	forever    = time.Duration(-1)
	servedIcon = "icon: 200 image/png; public, max-age=31536000, immutable; default-src 'none'; sandbox; named by its sha256: true; png: true"
)

type ending int

const (
	streamEnded ending = iota
	streamDropped
	streamRejected
	streamReplaced
)

type consumer struct {
	method                  string
	path                    string
	streamAuth              func(token string) string
	ackAuth                 func(token string) string
	ackMethod               string
	ackType                 string
	ackBody                 func(seq int64) string
	probes                  []string
	acks                    func(seq int64) []int64
	dropOnUnknown           bool
	reconnectDelay          time.Duration
	retriesAfter401         int
	reconnectsAfterReplaced int
}

type message struct {
	Seq  int64 `json:"seq"`
	Icon struct {
		Image string `json:"image"`
	} `json:"icon"`
}

type launcher struct {
	ctx        context.Context
	consumer   consumer
	mu         sync.Mutex
	tokens     []string
	tokenFiles []string
	files      []string
	stops      int
	transcript []string
}

func bearerHeader(token string) string { return "Bearer " + token }

func noHeader(string) string { return "" }

func wellBehaved() consumer {
	return consumer{
		method:         http.MethodGet,
		path:           "/consumer/v1/stream",
		streamAuth:     bearerHeader,
		ackAuth:        bearerHeader,
		ackMethod:      http.MethodPost,
		ackType:        "application/json",
		ackBody:        func(seq int64) string { return fmt.Sprintf(`{"seq": %d}`, seq) },
		acks:           func(seq int64) []int64 { return []int64{seq} },
		reconnectDelay: 300 * time.Millisecond,
	}
}

func (l *launcher) launch(url, tokenFile, token string) (func() error, error) {
	info, err := os.Stat(tokenFile)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.tokens = append(l.tokens, token)
	l.tokenFiles = append(l.tokenFiles, tokenFile)
	l.files = append(l.files, fmt.Sprintf("%v %s", info.Mode().Perm(), content))
	l.mu.Unlock()
	ctx, cancel := context.WithCancel(l.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.consumer.run(ctx, url, string(content), l)
	}()
	return func() error {
		cancel()
		<-done
		l.mu.Lock()
		defer l.mu.Unlock()
		l.stops++
		return nil
	}, nil
}

func (l *launcher) note(entry string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.transcript = append(l.transcript, entry)
}

func (l *launcher) acks() []string {
	return slices.DeleteFunc(slices.Clone(l.transcript), func(entry string) bool { return !strings.HasPrefix(entry, "ack ") })
}

func (c consumer) run(ctx context.Context, url, token string, l *launcher) {
	seen := map[ending]int{}
	for {
		end := c.connect(ctx, url, token, l)
		seen[end]++
		if !pause(ctx, c.wait(end, seen[end])) {
			return
		}
	}
}

func (c consumer) wait(end ending, times int) time.Duration {
	return map[ending]time.Duration{
		streamEnded:    c.reconnectDelay,
		streamDropped:  0,
		streamRejected: retry(times, c.retriesAfter401),
		streamReplaced: retry(times, c.reconnectsAfterReplaced),
	}[end]
}

func retry(times, allowed int) time.Duration {
	if times > allowed {
		return forever
	}
	return 0
}

func pause(ctx context.Context, d time.Duration) bool {
	if d == forever {
		<-ctx.Done()
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func setAuth(req *http.Request, value string) {
	if value != "" {
		req.Header.Set("Authorization", value)
	}
}

func (c consumer) connect(ctx context.Context, url, token string, l *launcher) ending {
	req, err := http.NewRequestWithContext(ctx, c.method, url+c.path, nil)
	if err != nil {
		return streamEnded
	}
	setAuth(req, c.streamAuth(token))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return streamEnded
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		l.note("stream: 401 " + resp.Header.Get("WWW-Authenticate"))
		return streamRejected
	}
	l.note(fmt.Sprintf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type")))
	if resp.StatusCode == http.StatusOK {
		c.probe(ctx, url, token, l)
	}
	return c.read(ctx, url, token, resp.Body, l)
}

func (c consumer) probe(ctx context.Context, url, token string, l *launcher) {
	for _, path := range c.probes {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+strings.ReplaceAll(path, "{token}", token), nil)
		if err != nil {
			return
		}
		setAuth(req, bearerHeader(token))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		_ = resp.Body.Close()
		l.note(fmt.Sprintf("probe %s: %d", path, resp.StatusCode))
	}
}

func (c consumer) read(ctx context.Context, url, token string, body io.Reader, l *launcher) ending {
	scanner := bufio.NewScanner(body)
	var lines []string
	caughtUp := false
	for scanner.Scan() {
		if scanner.Text() != "" {
			lines = append(lines, scanner.Text())
			continue
		}
		l.note(strings.Join(lines, "\n"))
		event, msg := parse(lines)
		lines = nil
		switch event {
		case "replaced":
			return streamReplaced
		case "caught_up":
			caughtUp = true
			c.ack(ctx, url, token, msg.Seq, l)
		case "notification":
			fetchIcon(ctx, url, msg.Icon.Image, l)
			if caughtUp {
				c.ack(ctx, url, token, msg.Seq, l)
			}
		default:
			if c.dropOnUnknown {
				return streamDropped
			}
		}
	}
	return streamEnded
}

func parse(lines []string) (string, message) {
	values := map[string]string{}
	for _, line := range lines {
		name, value, _ := strings.Cut(line, ": ")
		values[name] = value
	}
	var msg message
	_ = json.Unmarshal([]byte(values["data"]), &msg)
	return values["event"], msg
}

func fetchIcon(ctx context.Context, url, path string, l *launcher) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+path, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return
	}
	sum := sha256.Sum256(body)
	_, decodeErr := png.Decode(bytes.NewReader(body))
	h := resp.Header
	l.note(fmt.Sprintf("icon: %d %s; %s; %s; named by its sha256: %t; png: %t",
		resp.StatusCode, h.Get("Content-Type"), h.Get("Cache-Control"), h.Get("Content-Security-Policy"),
		path == "/icons/"+hex.EncodeToString(sum[:])+".png", decodeErr == nil))
}

func (c consumer) ack(ctx context.Context, url, token string, seq int64, l *launcher) {
	for _, n := range c.acks(seq) {
		req, err := http.NewRequestWithContext(ctx, c.ackMethod, url+"/consumer/v1/ack", strings.NewReader(c.ackBody(n)))
		if err != nil {
			return
		}
		if c.ackType != "" {
			req.Header.Set("Content-Type", c.ackType)
		}
		setAuth(req, c.ackAuth(token))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return
		}
		l.note(ackEntry(n, resp.StatusCode, body))
	}
}

func ackEntry(seq int64, status int, body []byte) string {
	if status != http.StatusOK {
		return fmt.Sprintf("ack %d: %d", seq, status)
	}
	return fmt.Sprintf("ack %d: %d %s", seq, status, bytes.TrimSpace(body))
}

func quick() consumertest.Options {
	return consumertest.Options{
		StartWithin:     time.Second,
		AckWithin:       time.Second,
		ReconnectWithin: time.Second,
		MinReconnectGap: 200 * time.Millisecond,
		QuietAfter:      300 * time.Millisecond,
	}
}

func runCheck(t *testing.T, c consumer, opts consumertest.Options) ([]string, *launcher) {
	t.Helper()
	l := &launcher{ctx: t.Context(), consumer: c}
	got, err := consumertest.Check(t.Context(), l.launch, opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return got, l
}

func expect(t *testing.T, name string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n%s\nwant:\n%s", name, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func notificationFrame(seq int, image string) string {
	icon := `{"emoji":"🔔","image":"` + image + `"}`
	return fmt.Sprintf(`id: %d
event: notification
data: {"seq":%d,"plugin":"conformance","display_name":"Consumer conformance","kind":"check","urgent":false,"title":"Conformance notification %d","body":"Sent by the consumer conformance check.","url":"https://example.com/conformance/%d","occurred_at":"2026-11-17T12:00:00Z","detected_at":"2026-11-17T12:00:14.25Z","icon":%s,"plugin_icon":%s}`, seq, seq, seq, seq, icon, icon)
}

func TestDefaultOptionsFollowTheContract(t *testing.T) {
	want := consumertest.Options{
		StartWithin:     30 * time.Second,
		AckWithin:       10 * time.Second,
		ReconnectWithin: 30 * time.Second,
		MinReconnectGap: time.Second,
		QuietAfter:      10 * time.Second,
	}
	if got := consumertest.DefaultOptions(); got != want {
		t.Errorf("DefaultOptions() = %+v, want %+v", got, want)
	}
}

func TestAWellBehavedConsumerPasses(t *testing.T) {
	t.Parallel()
	c := wellBehaved()
	c.reconnectDelay = 1200 * time.Millisecond
	opts := quick()
	opts.MinReconnectGap = time.Second
	opts.ReconnectWithin = 2 * time.Second
	got, l := runCheck(t, c, opts)
	expect(t, "violations", got)
	if len(l.tokens) != 2 || l.stops != 2 {
		t.Errorf("launched %d times and stopped %d times, want 2 and 2", len(l.tokens), l.stops)
	}
}

func TestAWellBehavedConsumerReceivesTheScript(t *testing.T) {
	t.Parallel()
	_, l := runCheck(t, wellBehaved(), quick())
	image := regexp.MustCompile(`/icons/[0-9a-f]{64}\.png`).FindString(strings.Join(l.transcript, "\n"))
	expect(t, "transcript", l.transcript,
		"stream: 200 text/event-stream",
		notificationFrame(1, image), servedIcon,
		notificationFrame(2, image), servedIcon,
		notificationFrame(3, image), servedIcon,
		"id: 3\nevent: caught_up\ndata: {\"seq\":3,\"backlog\":3}",
		`ack 3: 200 {"position":3}`,
		"event: future_thing\ndata: {\"x\":1}",
		notificationFrame(4, image), servedIcon,
		`ack 4: 200 {"position":4}`,
		"stream: 401 Bearer",
		"stream: 200 text/event-stream",
		"id: 4\nevent: caught_up\ndata: {\"seq\":4,\"backlog\":0}",
		`ack 4: 200 {"position":4}`,
		"event: replaced\ndata: {}",
	)
}

func TestEachRunGetsAFreshTokenInAPrivateFile(t *testing.T) {
	t.Parallel()
	_, l := runCheck(t, wellBehaved(), quick())
	pattern := regexp.MustCompile(`^mhc_[A-Za-z0-9_-]{43}$`)
	if len(l.tokens) != 2 || l.tokens[0] == l.tokens[1] {
		t.Fatalf("tokens = %q, want two different tokens", l.tokens)
	}
	for i, token := range l.tokens {
		if !pattern.MatchString(token) {
			t.Errorf("token %q does not match %s", token, pattern)
		}
		if want := "-rw------- " + token; l.files[i] != want {
			t.Errorf("token file of run %d = %q, want %q", i+1, l.files[i], want)
		}
	}
	for _, file := range l.tokenFiles {
		if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("token file %s after Check: %v, want it removed", file, err)
		}
	}
}

func TestAConsumerThatNeverAcksIsReported(t *testing.T) {
	t.Parallel()
	c := wellBehaved()
	c.acks = func(int64) []int64 { return nil }
	opts := quick()
	opts.AckWithin = 200 * time.Millisecond
	got, _ := runCheck(t, c, opts)
	expect(t, "violations", got,
		"no ack reached seq 3 within 200ms of caught_up",
		"no ack reached seq 4 within 200ms after an unknown event type",
	)
}

func TestAcksPastTheLastSeqOrBackwardsAreViolations(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		acks    func(seq int64) []int64
		want    []string
		replies []string
	}{
		"past the last seq": {
			acks: func(int64) []int64 { return []int64{9} },
			want: []string{
				"an ack went past the last seq sent: got 9, last sent 3",
				"no ack reached seq 3 within 200ms of caught_up",
				"an ack went past the last seq sent: got 9, last sent 4",
				"no ack reached seq 4 within 200ms after an unknown event type",
			},
			replies: []string{"ack 9: 422", "ack 9: 422"},
		},
		"backwards": {
			acks: func(seq int64) []int64 { return []int64{seq, seq - 1} },
			want: []string{
				"an ack went backwards: got 2 after 3",
				"an ack went backwards: got 3 after 4",
			},
			replies: []string{`ack 3: 200 {"position":3}`, `ack 2: 200 {"position":3}`},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := wellBehaved()
			c.acks = tt.acks
			opts := quick()
			opts.AckWithin = 200 * time.Millisecond
			got, l := runCheck(t, c, opts)
			expect(t, "violations", got, tt.want...)
			expect(t, "first replies", l.acks()[:min(len(l.acks()), len(tt.replies))], tt.replies...)
		})
	}
}

func TestAnAckTheHubRejectsIsReported(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		change func(*consumer)
		want   string
		reply  string
	}{
		"a method other than POST": {
			change: func(c *consumer) { c.ackMethod = http.MethodPut },
			want:   "an ack was sent with PUT, want POST",
			reply:  "ack 3: 405",
		},
		"no token": {
			change: func(c *consumer) { c.ackAuth = noHeader },
			want:   "an ack did not send the consumer token as a bearer token",
			reply:  "ack 3: 401",
		},
		"a form content type": {
			change: func(c *consumer) { c.ackType = "application/x-www-form-urlencoded" },
			want:   "an ack was sent with Content-Type application/x-www-form-urlencoded, want application/json",
			reply:  "ack 3: 415",
		},
		"a seq that is not an integer": {
			change: func(c *consumer) { c.ackBody = func(seq int64) string { return fmt.Sprintf(`{"seq": "%d"}`, seq) } },
			want:   `an ack body was not {"seq": <integer>}`,
			reply:  "ack 3: 422",
		},
		"an unknown field": {
			change: func(c *consumer) {
				c.ackBody = func(seq int64) string { return fmt.Sprintf(`{"seq": %d, "extra": 1}`, seq) }
			},
			want:  `an ack body was not {"seq": <integer>}`,
			reply: "ack 3: 422",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := wellBehaved()
			tt.change(&c)
			opts := quick()
			opts.AckWithin = 200 * time.Millisecond
			got, l := runCheck(t, c, opts)
			expect(t, "violations", got,
				tt.want,
				"no ack reached seq 3 within 200ms of caught_up",
				"no ack reached seq 4 within 200ms after an unknown event type",
			)
			expect(t, "first reply", l.acks()[:min(len(l.acks()), 1)], tt.reply)
		})
	}
}

func TestAnAckTheHubAcceptsPasses(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*consumer){
		"no content type":            func(c *consumer) { c.ackType = "" },
		"a charset parameter":        func(c *consumer) { c.ackType = "application/json; charset=utf-8" },
		"a +json type":               func(c *consumer) { c.ackType = "application/vnd.consumer+json" },
		"a content type in capitals": func(c *consumer) { c.ackType = "Application/JSON" },
		"the field name in capitals": func(c *consumer) { c.ackBody = func(seq int64) string { return fmt.Sprintf(`{"SEQ": %d}`, seq) } },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := wellBehaved()
			change(&c)
			got, l := runCheck(t, c, quick())
			expect(t, "violations", got)
			expect(t, "first reply", l.acks()[:min(len(l.acks()), 1)], `ack 3: 200 {"position":3}`)
		})
	}
}

func TestRequestsBesideTheStreamAndTheAck(t *testing.T) {
	t.Parallel()
	c := wellBehaved()
	c.probes = []string{
		"/healthz",
		"/icons/" + strings.Repeat("0", 64) + ".png",
		"/consumer/v1/status",
		"/consumer/v1/{token}%0A",
	}
	got, l := runCheck(t, c, quick())
	expect(t, "violations", got,
		`a request went to GET "/consumer/v1/status", which the hub does not serve`,
		`a request went to GET "/consumer/v1/<token>\n", which the hub does not serve`,
	)
	probes := slices.DeleteFunc(slices.Clone(l.transcript), func(entry string) bool { return !strings.HasPrefix(entry, "probe ") })
	expect(t, "first probes", probes[:min(len(probes), 4)],
		"probe /healthz: 200",
		"probe /icons/"+strings.Repeat("0", 64)+".png: 404",
		"probe /consumer/v1/status: 404",
		"probe /consumer/v1/{token}%0A: 404",
	)
	expect(t, "acks", l.acks(), `ack 3: 200 {"position":3}`, `ack 4: 200 {"position":4}`, `ack 4: 200 {"position":4}`)
}

func TestAWrongPathOrMissingTokenIsReported(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		change func(*consumer)
		want   []string
	}{
		"a wrong method": {
			change: func(c *consumer) { c.method = http.MethodPost },
			want:   []string{"the stream request was sent with POST, want GET"},
		},
		"a wrong path": {
			change: func(c *consumer) { c.path = "/stream" },
			want: []string{
				`a request went to GET "/stream", which the hub does not serve`,
				"the consumer did not open the stream within 1s",
			},
		},
		"no token": {
			change: func(c *consumer) { c.streamAuth = noHeader },
			want:   []string{"the stream request did not send the consumer token as a bearer token"},
		},
		"the token under another scheme": {
			change: func(c *consumer) { c.streamAuth = func(token string) string { return "Token " + token } },
			want:   []string{"the stream request did not send the consumer token as a bearer token"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := wellBehaved()
			tt.change(&c)
			got, _ := runCheck(t, c, quick())
			expect(t, "violations", got, tt.want...)
		})
	}
}

func TestAnUnknownEventThatDropsTheConnectionIsReported(t *testing.T) {
	t.Parallel()
	c := wellBehaved()
	c.dropOnUnknown = true
	got, _ := runCheck(t, c, quick())
	expect(t, "violations", got, "an unknown event type made the consumer reconnect")
}

func TestReconnectingTooSoonIsReported(t *testing.T) {
	t.Parallel()
	c := wellBehaved()
	c.reconnectDelay = 0
	got, _ := runCheck(t, c, quick())
	pattern := regexp.MustCompile(`^the consumer reconnected after [0-9]+m?s, sooner than 200ms$`)
	if len(got) != 1 || !pattern.MatchString(got[0]) {
		t.Errorf("violations = %q, want one matching %s", got, pattern)
	}
}

func TestAConsumerThatDoesNotReconnectIsReported(t *testing.T) {
	t.Parallel()
	c := wellBehaved()
	c.reconnectDelay = forever
	opts := quick()
	opts.ReconnectWithin = 300 * time.Millisecond
	got, _ := runCheck(t, c, opts)
	expect(t, "violations", got, "the consumer did not reconnect within 300ms after the stream ended")
}

func TestHammeringAfterARejectionIsAViolation(t *testing.T) {
	t.Parallel()
	c := wellBehaved()
	c.retriesAfter401 = 3
	got, _ := runCheck(t, c, quick())
	expect(t, "violations", got, "the consumer made 3 stream requests within 300ms after a 401, want at most 1")
}

func TestReconnectingAfterReplacedIsAViolation(t *testing.T) {
	t.Parallel()
	c := wellBehaved()
	c.reconnectsAfterReplaced = 1
	got, _ := runCheck(t, c, quick())
	expect(t, "violations", got, "the consumer made 1 stream request within 300ms after replaced, want none")
}

func TestAConsumerThatNeverConnectsStopsTheScript(t *testing.T) {
	t.Parallel()
	var launches, stops int
	launch := func(string, string, string) (func() error, error) {
		launches++
		return func() error {
			stops++
			return nil
		}, nil
	}
	opts := quick()
	opts.StartWithin = 100 * time.Millisecond
	got, err := consumertest.Check(t.Context(), launch, opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	expect(t, "violations", got, "the consumer did not open the stream within 100ms")
	if launches != 1 || stops != 1 {
		t.Errorf("launched %d times and stopped %d times, want 1 and 1", launches, stops)
	}
}

func TestALaunchErrorIsAnError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	launch := func(string, string, string) (func() error, error) { return nil, boom }
	got, err := consumertest.Check(t.Context(), launch, quick())
	if !errors.Is(err, boom) {
		t.Errorf("Check error = %v, want %v", err, boom)
	}
	if got != nil {
		t.Errorf("violations = %q, want none", got)
	}
}

func TestCancelingTheContextStopsTheConsumer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	stopped := false
	launch := func(string, string, string) (func() error, error) {
		cancel()
		return func() error {
			stopped = true
			return nil
		}, nil
	}
	got, err := consumertest.Check(ctx, launch, consumertest.DefaultOptions())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Check error = %v, want %v", err, context.Canceled)
	}
	if got != nil || !stopped {
		t.Errorf("violations = %q and stopped = %v, want none and true", got, stopped)
	}
}
