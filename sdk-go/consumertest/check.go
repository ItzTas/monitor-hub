package consumertest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Options struct {
	StartWithin     time.Duration
	AckWithin       time.Duration
	ReconnectWithin time.Duration
	MinReconnectGap time.Duration
	QuietAfter      time.Duration
}

type Launch func(url, tokenFile, token string) (stop func() error, err error)

type outcome int

const (
	arrived outcome = iota
	reached
	expired
	canceled
)

type icon struct {
	Emoji string `json:"emoji"`
	Image string `json:"image"`
}

type notification struct {
	Seq         int64  `json:"seq"`
	Plugin      string `json:"plugin"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
	Urgent      bool   `json:"urgent"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	URL         string `json:"url"`
	OccurredAt  string `json:"occurred_at"`
	DetectedAt  string `json:"detected_at"`
	Icon        icon   `json:"icon"`
	PluginIcon  icon   `json:"plugin_icon"`
}

type caughtUp struct {
	Seq     int64 `json:"seq"`
	Backlog int   `json:"backlog"`
}

type script struct {
	ctx       context.Context
	hub       *hub
	opts      Options
	launch    Launch
	tokenFile string
}

func DefaultOptions() Options {
	return Options{
		StartWithin:     30 * time.Second,
		AckWithin:       10 * time.Second,
		ReconnectWithin: 30 * time.Second,
		MinReconnectGap: time.Second,
		QuietAfter:      10 * time.Second,
	}
}

func Check(ctx context.Context, launch Launch, opts Options) ([]string, error) {
	dir, err := os.MkdirTemp("", "consumertest-")
	if err != nil {
		return nil, fmt.Errorf("create the token directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	h, err := listen(ctx)
	if err != nil {
		return nil, err
	}
	defer h.close()
	s := &script{ctx: ctx, hub: h, opts: opts, launch: launch, tokenFile: filepath.Join(dir, "token")}
	if err := s.play(); err != nil {
		return nil, err
	}
	return h.report.violations(), nil
}

func (s *script) play() error {
	for _, body := range []func(*run) bool{s.firstRun, s.secondRun} {
		more, err := s.execute(body)
		if err != nil || !more {
			return err
		}
	}
	return nil
}

func (s *script) execute(body func(*run) bool) (bool, error) {
	token, err := newToken()
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(s.tokenFile, []byte(token+"\n"), 0o600); err != nil {
		return false, fmt.Errorf("write the token file: %w", err)
	}
	current := s.hub.begin(token)
	stop, err := s.launch(s.hub.url, s.tokenFile, token)
	if err != nil {
		return false, fmt.Errorf("launch the consumer: %w", err)
	}
	more := body(current)
	_ = stop()
	return more, s.ctx.Err()
}

func (s *script) firstRun(current *run) bool {
	conn, ok := s.connect(current)
	if !ok {
		return false
	}
	if conn, ok = s.catchUp(current, conn); !ok {
		return false
	}
	if conn, ok = s.unknownEvent(current, conn); !ok {
		return false
	}
	return s.reconnect(current, conn)
}

func (s *script) secondRun(current *run) bool {
	conn, ok := s.connect(current)
	if !ok {
		return false
	}
	s.send(conn, 4, "caught_up", caughtUp{Seq: 4})
	s.send(conn, 0, "replaced", json.RawMessage(`{}`))
	conn.end()
	n, ok := s.count(current, func(req *request) { req.do(openStream) })
	if n > 0 {
		s.hub.report.add("the consumer made %s within %s after replaced, want none", streamRequests(n), s.opts.QuietAfter)
	}
	return ok
}

func (s *script) connect(current *run) (*request, bool) {
	req, out := s.nextRequest(current, time.After(s.opts.StartWithin))
	s.reportExpiry(out, "the consumer did not open the stream within %s", s.opts.StartWithin)
	if out != arrived {
		return nil, false
	}
	req.do(openStream)
	return req, true
}

func (s *script) catchUp(current *run, conn *request) (*request, bool) {
	for _, seq := range []int64{1, 2, 3} {
		s.send(conn, seq, "notification", scripted(seq))
	}
	s.send(conn, 3, "caught_up", caughtUp{Seq: 3, Backlog: 3})
	deadline := time.After(s.opts.AckWithin)
	for {
		req, out := s.await(current, 3, deadline)
		s.reportExpiry(out, "no ack reached seq 3 within %s of caught_up", s.opts.AckWithin)
		if out != arrived {
			return conn, out != canceled
		}
		conn = replace(conn, req)
	}
}

func (s *script) unknownEvent(current *run, conn *request) (*request, bool) {
	s.send(conn, 0, "future_thing", json.RawMessage(`{"x":1}`))
	s.send(conn, 4, "notification", scripted(4))
	req, out := s.await(current, 4, time.After(s.opts.AckWithin))
	s.reportExpiry(out, "no ack reached seq 4 within %s after an unknown event type", s.opts.AckWithin)
	if out != arrived {
		return conn, out != canceled
	}
	s.hub.report.add("an unknown event type made the consumer reconnect")
	return replace(conn, req), true
}

func (s *script) reconnect(current *run, conn *request) bool {
	conn.end()
	ended := time.Now()
	req, out := s.nextRequest(current, time.After(s.opts.ReconnectWithin))
	s.reportExpiry(out, "the consumer did not reconnect within %s after the stream ended", s.opts.ReconnectWithin)
	if out != arrived {
		return out != canceled
	}
	if gap := max(req.at.Sub(ended), 0); gap < s.opts.MinReconnectGap {
		s.hub.report.add("the consumer reconnected after %s, sooner than %s", gap.Truncate(time.Millisecond), s.opts.MinReconnectGap)
	}
	reject(req)
	n, ok := s.count(current, reject)
	if n > 1 {
		s.hub.report.add("the consumer made %s within %s after a 401, want at most 1", streamRequests(n), s.opts.QuietAfter)
	}
	return ok
}

func (s *script) send(conn *request, seq int64, event string, data any) {
	s.hub.announce(seq)
	conn.do(message(seq, event, data))
}

func (s *script) reportExpiry(out outcome, format string, args ...any) {
	if out == expired {
		s.hub.report.add(format, args...)
	}
}

func (s *script) nextRequest(current *run, deadline <-chan time.Time) (*request, outcome) {
	select {
	case req := <-current.requests:
		return req, arrived
	case <-deadline:
		return nil, expired
	case <-s.ctx.Done():
		return nil, canceled
	}
}

func (s *script) await(current *run, seq int64, deadline <-chan time.Time) (*request, outcome) {
	for {
		done, moved := s.hub.acked(current, seq)
		if done {
			return nil, reached
		}
		select {
		case req := <-current.requests:
			return req, arrived
		case <-moved:
		case <-deadline:
			return nil, expired
		case <-s.ctx.Done():
			return nil, canceled
		}
	}
}

func (s *script) count(current *run, answer func(*request)) (int, bool) {
	deadline := time.After(s.opts.QuietAfter)
	for n := 0; ; n++ {
		req, out := s.nextRequest(current, deadline)
		if out != arrived {
			return n, out != canceled
		}
		answer(req)
	}
}

func replace(conn, req *request) *request {
	conn.end()
	req.do(openStream)
	return req
}

func reject(req *request) {
	req.do(rejectToken)
	req.end()
}

func streamRequests(n int) string {
	if n == 1 {
		return "1 stream request"
	}
	return fmt.Sprintf("%d stream requests", n)
}

func newToken() (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	return "mhc_" + base64.RawURLEncoding.EncodeToString(secret), nil
}

func scripted(seq int64) notification {
	bell := icon{Emoji: "🔔", Image: iconPath}
	return notification{
		Seq:         seq,
		Plugin:      "conformance",
		DisplayName: "Consumer conformance",
		Kind:        "check",
		Title:       fmt.Sprintf("Conformance notification %d", seq),
		Body:        "Sent by the consumer conformance check.",
		URL:         fmt.Sprintf("https://example.com/conformance/%d", seq),
		OccurredAt:  "2026-11-17T12:00:00Z",
		DetectedAt:  "2026-11-17T12:00:14.25Z",
		Icon:        bell,
		PluginIcon:  bell,
	}
}
