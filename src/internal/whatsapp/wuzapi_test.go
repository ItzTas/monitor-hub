package whatsapp

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/src/internal/core"
)

const (
	wuzapiToken     = "fake-wuzapi-user-token"
	wuzapiUserAgent = "monitor-hub/test"
	wuzapiText      = "monitor-hub: test message\n\nThis is a test message from monitor-hub."
)

type wuzapiAnswer struct {
	status int
	body   string
}

type wuzapiRequest struct {
	method string
	path   string
	header http.Header
	body   string
}

// fakeWuzAPI answers each path with its answer and records every request.
type fakeWuzAPI struct {
	server   *httptest.Server
	mu       sync.Mutex
	answers  map[string]wuzapiAnswer
	requests []wuzapiRequest
}

func newFakeWuzAPI(t *testing.T, answers map[string]wuzapiAnswer) *fakeWuzAPI {
	t.Helper()
	fake := &fakeWuzAPI{answers: answers}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fake.mu.Lock()
		fake.requests = append(fake.requests, wuzapiRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: string(body)})
		answer, ok := fake.answers[r.URL.Path]
		fake.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(answer.status)
		_, _ = io.WriteString(w, answer.body)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeWuzAPI) answer(path string, answer wuzapiAnswer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[path] = answer
}

func (f *fakeWuzAPI) received() []wuzapiRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func newWuzAPI(address string, logs io.Writer) *WuzAPI {
	return NewWuzAPI(core.WuzAPI{URL: address, Token: core.NewSecret(wuzapiToken)}, wuzapiUserAgent, slog.New(slog.NewTextHandler(logs, nil)))
}

func sessionStatus(connected, loggedIn bool) wuzapiAnswer {
	body := map[[2]bool]string{
		{true, true}:   `{"code":200,"data":{"connected":true,"loggedIn":true,"jid":"550000000001:7@s.whatsapp.net"},"success":true}`,
		{true, false}:  `{"code":200,"data":{"connected":true,"loggedIn":false},"success":true}`,
		{false, false}: `{"code":200,"data":{"connected":false,"loggedIn":false},"success":true}`,
	}[[2]bool{connected, loggedIn}]
	return wuzapiAnswer{http.StatusOK, body}
}

func TestWuzAPIIsReadyWhenItsSessionIsConnectedAndLoggedIn(t *testing.T) {
	cases := map[string]struct {
		answer wuzapiAnswer
		closed bool
		ready  bool
		reason string
	}{
		"connected and logged in":  {answer: sessionStatus(true, true), ready: true},
		"connected, not logged in": {answer: sessionStatus(true, false), reason: `reason="the WuzAPI session is not logged in; pair the number in WuzAPI"`},
		"not connected":            {answer: sessionStatus(false, false), reason: `reason="the WuzAPI session is not connected"`},
		"token refused":            {answer: wuzapiAnswer{http.StatusUnauthorized, `{"code":401,"error":"unauthorized","success":false}`}, reason: `reason="WuzAPI refused the token (401); check WHATSAPP_WUZAPI_TOKEN"`},
		"proxy error page":         {answer: wuzapiAnswer{http.StatusInternalServerError, "<html><body>Internal Server Error</body></html>"}, reason: `reason="WuzAPI answered 500"`},
		"server down":              {closed: true, reason: `reason="could not reach the server: `},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newFakeWuzAPI(t, map[string]wuzapiAnswer{"/session/status": tc.answer})
			if tc.closed {
				fake.server.Close()
			}
			var logs bytes.Buffer

			ready := newWuzAPI(fake.server.URL, &logs).Ready(t.Context())

			if ready != tc.ready || (tc.reason == "" && logs.Len() != 0) || !strings.Contains(logs.String(), tc.reason) {
				t.Errorf("Ready = %v, want %v; logs:\n%s", ready, tc.ready, logs.String())
			}
			if tc.closed {
				return
			}
			got := fake.received()
			if len(got) != 1 || got[0].method != http.MethodGet || got[0].path != "/session/status" || got[0].header.Get("Token") != wuzapiToken || got[0].header.Get("User-Agent") != wuzapiUserAgent {
				t.Errorf("%d requests; the first as expected: %v", len(got), len(got) == 1 && got[0].method == http.MethodGet && got[0].path == "/session/status" && got[0].header.Get("Token") == wuzapiToken && got[0].header.Get("User-Agent") == wuzapiUserAgent)
			}
		})
	}
}

func TestWuzAPINotReadyAnswersAtOnceForAMinute(t *testing.T) {
	fake := newFakeWuzAPI(t, map[string]wuzapiAnswer{"/session/status": sessionStatus(false, false)})
	gateway := newWuzAPI(fake.server.URL, io.Discard)
	t0 := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	now := t0
	gateway.now = func() time.Time { return now }

	first := gateway.Ready(t.Context())
	fake.answer("/session/status", sessionStatus(true, true))
	now = t0.Add(time.Minute - time.Nanosecond)
	within := gateway.Ready(t.Context())
	requestsWithin := len(fake.received())
	now = t0.Add(time.Minute)
	after := gateway.Ready(t.Context())

	if first || within || requestsWithin != 1 || !after || len(fake.received()) != 2 {
		t.Errorf("Ready = %v, then %v after %d requests, then %v after %d requests", first, within, requestsWithin, after, len(fake.received()))
	}
}

func TestWuzAPIReadyGivesUpAfterItsTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	gateway := newWuzAPI(server.URL, io.Discard)
	gateway.readyTimeout = 20 * time.Millisecond

	done := make(chan bool, 1)
	go func() { done <- gateway.Ready(t.Context()) }()

	select {
	case ready := <-done:
		if ready {
			t.Error("Ready = true for a WuzAPI that never answers")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ready still waiting after 5s")
	}
}

func TestWuzAPILogsReadinessOnlyWhenItChanges(t *testing.T) {
	fake := newFakeWuzAPI(t, map[string]wuzapiAnswer{"/session/status": sessionStatus(false, false)})
	var logs bytes.Buffer
	gateway := newWuzAPI(fake.server.URL, &logs)
	t0 := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	now := t0
	gateway.now = func() time.Time { return now }

	gateway.Ready(t.Context())
	now = t0.Add(time.Minute)
	gateway.Ready(t.Context())
	fake.answer("/session/status", sessionStatus(true, true))
	now = t0.Add(2 * time.Minute)
	gateway.Ready(t.Context())
	gateway.Ready(t.Context())

	warn := `level=WARN msg="WhatsApp notifications wait because WuzAPI is not ready" reason="the WuzAPI session is not connected"`
	info := `level=INFO msg="WuzAPI is ready; WhatsApp notifications resume"`
	if strings.Count(logs.String(), warn) != 1 || strings.Count(logs.String(), info) != 1 || strings.Count(logs.String(), "\n") != 2 || len(fake.received()) != 4 {
		t.Errorf("%d requests, logs:\n%s", len(fake.received()), logs.String())
	}
}

func TestWuzAPILooksNumbersUp(t *testing.T) {
	cases := map[string]struct {
		answer string
		chat   string
		err    error
	}{
		"on WhatsApp": {
			answer: `{"code":200,"data":{"Users":[{"Query":"+5500900000001","IsInWhatsapp":true,"JID":"550000000001@s.whatsapp.net","VerifiedName":""}]},"success":true}`,
			chat:   numberChat,
		},
		"not on WhatsApp": {
			answer: `{"code":200,"data":{"Users":[{"Query":"+5500900000001","IsInWhatsapp":false,"JID":"5500900000001@s.whatsapp.net","VerifiedName":""}]},"success":true}`,
			err:    ErrNotOnWhatsApp,
		},
		"no user": {
			answer: `{"code":200,"data":{"Users":null},"success":true}`,
			err:    ErrNotOnWhatsApp,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newFakeWuzAPI(t, map[string]wuzapiAnswer{"/user/check": {http.StatusOK, tc.answer}})

			chat, err := newWuzAPI(fake.server.URL, io.Discard).Lookup(t.Context(), number)

			if chat != tc.chat || !errors.Is(err, tc.err) {
				t.Errorf("Lookup returned the expected chat: %v; error as expected: %v", chat == tc.chat, errors.Is(err, tc.err))
			}
			got := fake.received()
			if len(got) != 1 || got[0].method != http.MethodPost || got[0].body != `{"Phone":["+5500900000001"]}` || got[0].header.Get("Token") != wuzapiToken || got[0].header.Get("Content-Type") != "application/json" {
				t.Errorf("%d requests; the first as expected: %v", len(got), len(got) == 1 && got[0].method == http.MethodPost && got[0].body == `{"Phone":["+5500900000001"]}`)
			}
		})
	}
}

func TestWuzAPISendsText(t *testing.T) {
	for _, chat := range []string{numberChat, group} {
		fake := newFakeWuzAPI(t, map[string]wuzapiAnswer{"/chat/send/text": {http.StatusOK, `{"code":200,"data":{"Details":"Sent","Id":"3EB0000000000000000000","Timestamp":1764331200},"success":true}`}})

		err := newWuzAPI(fake.server.URL, io.Discard).SendText(t.Context(), chat, wuzapiText)

		got := fake.received()
		want := `{"Phone":"` + chat + `","Body":"monitor-hub: test message\n\nThis is a test message from monitor-hub."}`
		if err != nil || len(got) != 1 || got[0].method != http.MethodPost || got[0].body != want || got[0].header.Get("Token") != wuzapiToken || got[0].header.Get("Content-Type") != "application/json" || got[0].header.Get("User-Agent") != wuzapiUserAgent {
			t.Errorf("group %v: SendText = %v, %d requests; the first as expected: %v", chat == group, redact(errText(err)), len(got), len(got) == 1 && got[0].body == want)
		}
	}
}

func TestWuzAPIAnswersThatAreNotASuccessAreErrors(t *testing.T) {
	long := strings.Repeat("é", 150)
	cases := map[string]struct {
		answer wuzapiAnswer
		closed bool
		want   string
	}{
		"proxy error page":       {answer: wuzapiAnswer{http.StatusBadGateway, "<html><body>502 Bad Gateway</body></html>"}, want: "WuzAPI answered 502"},
		"token refused":          {answer: wuzapiAnswer{http.StatusUnauthorized, `{"code":401,"error":"unauthorized","success":false}`}, want: "WuzAPI refused the token (401); check WHATSAPP_WUZAPI_TOKEN"},
		"error with a reason":    {answer: wuzapiAnswer{http.StatusInternalServerError, `{"code":500,"error":"no session","success":false}`}, want: "WuzAPI answered 500: no session"},
		"long reason":            {answer: wuzapiAnswer{http.StatusBadRequest, `{"code":400,"error":"` + long + `","success":false}`}, want: "WuzAPI answered 400: " + strings.Repeat("é", 100)},
		"success false":          {answer: wuzapiAnswer{http.StatusOK, `{"code":200,"error":"failed to send","success":false}`}, want: "WuzAPI answered 200 but reported a failure: failed to send"},
		"success false, no text": {answer: wuzapiAnswer{http.StatusOK, `{"code":200,"success":false}`}, want: "WuzAPI answered 200 but reported a failure"},
		"not JSON":               {answer: wuzapiAnswer{http.StatusOK, "OK"}, want: "WuzAPI sent an answer monitor-hub could not read"},
		"no data":                {answer: wuzapiAnswer{http.StatusOK, `{"code":200,"success":true}`}, want: "WuzAPI sent an answer monitor-hub could not read"},
		"null data":              {answer: wuzapiAnswer{http.StatusOK, `{"code":200,"data":null,"success":true}`}, want: "WuzAPI sent an answer monitor-hub could not read"},
		"data of another shape":  {answer: wuzapiAnswer{http.StatusOK, `{"code":200,"data":["x"],"success":true}`}, want: "WuzAPI sent an answer monitor-hub could not read"},
		"server down":            {closed: true, want: "could not reach the server: "},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			paths := map[string]wuzapiAnswer{"/session/status": tc.answer, "/user/check": tc.answer, "/chat/send/text": tc.answer}
			fake := newFakeWuzAPI(t, paths)
			if tc.closed {
				fake.server.Close()
			}
			gateway := newWuzAPI(fake.server.URL, io.Discard)

			ready := gateway.Ready(t.Context())
			chat, lookupErr := gateway.Lookup(t.Context(), number)
			sendErr := gateway.SendText(t.Context(), numberChat, wuzapiText)

			if ready || chat != "" || lookupErr == nil || !strings.Contains(lookupErr.Error(), tc.want) || errors.Is(lookupErr, ErrNotOnWhatsApp) {
				t.Errorf("Ready = %v, Lookup gave a chat: %v, error %v", ready, chat != "", redact(errText(lookupErr)))
			}
			if sendErr == nil || !strings.HasPrefix(sendErr.Error(), tc.want) || (!tc.closed && sendErr.Error() != tc.want) {
				t.Errorf("SendText error:\n%v\nwant:\n%s", redact(errText(sendErr)), tc.want)
			}
		})
	}
}

func TestWuzAPIErrorsNeverShowTheTokenNumbersOrText(t *testing.T) {
	echo := func(detail string) wuzapiAnswer {
		return wuzapiAnswer{http.StatusBadRequest, `{"code":400,"error":"bad request from ` + wuzapiToken + detail + `","success":false}`}
	}
	fake := newFakeWuzAPI(t, map[string]wuzapiAnswer{
		"/session/status": echo(""),
		"/user/check":     echo(" for +5500900000001"),
		"/chat/send/text": echo(" for 550000000001@s.whatsapp.net: " + strings.ReplaceAll(wuzapiText, "\n", `\n`)),
	})
	var logs bytes.Buffer
	gateway := newWuzAPI(fake.server.URL, &logs)

	gateway.Ready(t.Context())
	_, lookupErr := gateway.Lookup(t.Context(), number)
	sendErr := gateway.SendText(t.Context(), numberChat, wuzapiText)

	outputs := map[string]string{"the log": logs.String(), "the Lookup error": errText(lookupErr), "the SendText error": errText(sendErr)}
	hidden := map[string]string{"the token": wuzapiToken, "the number": "5500900000001", "the chat": "550000000001", "the text": "test message"}
	for where, output := range outputs {
		if !strings.Contains(output, "WuzAPI answered 400: bad request from [redacted]") {
			t.Errorf("%s does not say what WuzAPI answered", where)
		}
		for what, value := range hidden {
			if strings.Contains(output, value) {
				t.Errorf("%s shows %s", where, what)
			}
		}
	}
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
