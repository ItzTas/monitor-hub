package consumertest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	streamPath   = "/consumer/v1/stream"
	ackPath      = "/consumer/v1/ack"
	iconsPrefix  = "/icons/"
	invalidToken = "missing or invalid consumer token"
)

type action func(http.ResponseWriter, *http.ResponseController) error

type request struct {
	at      time.Time
	actions chan action
	gone    chan struct{}
}

type run struct {
	token    string
	requests chan *request
	over     chan struct{}
	position int64
	moved    chan struct{}
}

type report struct {
	mu   sync.Mutex
	list []string
}

type hub struct {
	server   *http.Server
	url      string
	served   chan struct{}
	report   report
	handlers sync.WaitGroup

	mu       sync.Mutex
	closing  bool
	current  *run
	lastSent int64
}

func newRun(token string) *run {
	return &run{
		token:    token,
		requests: make(chan *request),
		over:     make(chan struct{}),
		moved:    make(chan struct{}),
	}
}

func listen(ctx context.Context) (*hub, error) {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for the consumer: %w", err)
	}
	h := &hub{
		url:     "http://" + listener.Addr().String(),
		served:  make(chan struct{}),
		current: newRun(""),
	}
	h.server = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		defer close(h.served)
		_ = h.server.Serve(listener)
	}()
	return h, nil
}

func (h *hub) begin(token string) *run {
	h.mu.Lock()
	defer h.mu.Unlock()
	close(h.current.over)
	h.current = newRun(token)
	return h.current
}

func (h *hub) close() {
	h.mu.Lock()
	h.closing = true
	close(h.current.over)
	h.mu.Unlock()
	_ = h.server.Close()
	h.handlers.Wait()
	<-h.served
}

func (h *hub) enter() (*run, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return nil, false
	}
	h.handlers.Add(1)
	return h.current, true
}

func (h *hub) announce(seq int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastSent = max(h.lastSent, seq)
}

func (h *hub) acked(r *run, seq int64) (bool, <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return r.position >= seq, r.moved
}

func (h *hub) acknowledge(r *run, seq int64) (int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if seq > h.lastSent {
		h.report.add("an ack went past the last seq sent: got %d, last sent %d", seq, h.lastSent)
		return 0, false
	}
	if seq < r.position {
		h.report.add("an ack went backwards: got %d after %d", seq, r.position)
		return r.position, true
	}
	r.position = seq
	close(r.moved)
	r.moved = make(chan struct{})
	return seq, true
}

func (h *hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	current, ok := h.enter()
	if !ok {
		problem(w, http.StatusServiceUnavailable, "the conformance check is over")
		return
	}
	defer h.handlers.Done()
	if r.URL.Path == ackPath {
		h.ack(w, r, current)
		return
	}
	if strings.HasPrefix(r.URL.Path, iconsPrefix) {
		http.NotFound(w, r)
		return
	}
	h.stream(w, r, current)
}

func (h *hub) ack(w http.ResponseWriter, r *http.Request, current *run) {
	if r.Method != http.MethodPost {
		h.report.add("an ack was sent with %s, want POST", r.Method)
		w.Header().Set("Allow", http.MethodPost)
		problem(w, http.StatusMethodNotAllowed, "the ack is sent with POST")
		return
	}
	if !hasToken(r, current.token) {
		h.report.add("an ack did not send the consumer token as a bearer token")
		unauthorized(w)
		return
	}
	if contentType := r.Header.Get("Content-Type"); !isJSON(contentType) {
		h.report.add("an ack was sent with Content-Type %s, want application/json", contentType)
		problem(w, http.StatusUnsupportedMediaType, "the body must be application/json")
		return
	}
	seq, ok := readSeq(r.Body)
	if !ok {
		h.report.add(`an ack body was not {"seq": <integer>}`)
		problem(w, http.StatusUnprocessableEntity, `the body must be {"seq": <integer>}`)
		return
	}
	position, ok := h.acknowledge(current, seq)
	if !ok {
		problem(w, http.StatusUnprocessableEntity, "seq is beyond the last event sent")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Position int64 `json:"position"`
	}{position})
}

func (h *hub) stream(w http.ResponseWriter, r *http.Request, current *run) {
	if r.Method != http.MethodGet || r.URL.Path != streamPath {
		h.report.add("the stream request was %s %s, want GET %s", r.Method, r.URL.Path, streamPath)
	}
	if !hasToken(r, current.token) {
		h.report.add("the stream request did not send the consumer token as a bearer token")
	}
	req := &request{at: time.Now(), actions: make(chan action), gone: make(chan struct{})}
	defer close(req.gone)
	select {
	case current.requests <- req:
	case <-current.over:
		problem(w, http.StatusServiceUnavailable, "the conformance run is over")
		return
	case <-r.Context().Done():
		return
	}
	rc := http.NewResponseController(w)
	for {
		select {
		case act, ok := <-req.actions:
			if !ok || act(w, rc) != nil {
				return
			}
		case <-current.over:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (q *request) do(act action) {
	select {
	case q.actions <- act:
	case <-q.gone:
	}
}

func (q *request) end() {
	close(q.actions)
}

func openStream(w http.ResponseWriter, rc *http.ResponseController) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	return rc.Flush()
}

func rejectToken(w http.ResponseWriter, _ *http.ResponseController) error {
	unauthorized(w)
	return nil
}

func message(id int64, event string, data any) action {
	return func(w http.ResponseWriter, rc *http.ResponseController) error {
		body, err := json.Marshal(data)
		if err != nil {
			return err
		}
		var frame []byte
		if id > 0 {
			frame = fmt.Appendf(frame, "id: %d\n", id)
		}
		frame = fmt.Appendf(frame, "event: %s\ndata: %s\n\n", event, body)
		if _, err := w.Write(frame); err != nil {
			return err
		}
		return rc.Flush()
	}
}

func hasToken(r *http.Request, token string) bool {
	scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	return ok && strings.EqualFold(scheme, "Bearer") && value == token
}

func isJSON(contentType string) bool {
	start := strings.IndexByte(contentType, '+') + 1
	end := strings.IndexByte(contentType, ';')
	if end < 0 {
		end = len(contentType)
	}
	if end < start {
		return false
	}
	return slices.Contains([]string{"", "application/json", "json"}, strings.ToLower(contentType[start:end]))
}

func readSeq(body io.Reader) (int64, bool) {
	decoder := json.NewDecoder(io.LimitReader(body, 1<<10))
	decoder.DisallowUnknownFields()
	var ack struct {
		Seq *int64 `json:"seq"`
	}
	if decoder.Decode(&ack) != nil || ack.Seq == nil || *ack.Seq < 0 {
		return 0, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return 0, false
	}
	return *ack.Seq, true
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	problem(w, http.StatusUnauthorized, invalidToken)
}

func problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}{http.StatusText(status), status, detail})
}

func (r *report) add(format string, args ...any) {
	violation := fmt.Sprintf(format, args...)
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.Contains(r.list, violation) {
		return
	}
	r.list = append(r.list, violation)
}

func (r *report) violations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.list)
}
