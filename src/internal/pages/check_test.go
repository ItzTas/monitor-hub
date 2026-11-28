package pages

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/ItzTas/monitor-hub/src/internal/api/monitors"
)

func TestCheckNow(t *testing.T) {
	logs := &lockedBuffer{}
	mux, _, monitored := newSaveSite(t, Deps{Log: slog.New(slog.NewTextHandler(logs, nil))})
	check := func(options ...option) (int, string) {
		rec := serve(t.Context(), mux, http.MethodPost, "/settings/sei/check", "", append([]option{fromHub, withCookie(cookie("admin"))}, options...)...)
		return rec.Code, text(rec)
	}
	answers := []struct {
		name   string
		result monitors.CheckResult
		err    error
		status int
		want   []string
	}{
		{"accepted", monitors.CheckResult{Accepted: true, Message: "queued"}, nil, http.StatusOK, []string{"The monitor started a check.", "queued"}},
		{"refused", monitors.CheckResult{Message: "refused while paused"}, nil, http.StatusOK, []string{"The monitor did not start a check.", "refused while paused"}},
		{"not running", monitors.CheckResult{}, monitors.ErrNotRunning, http.StatusOK, []string{"The monitor is not running, so it cannot check now."}},
		{"unknown", monitors.CheckResult{}, monitors.ErrUnknown, http.StatusNotFound, []string{textUnknown}},
		{"failed", monitors.CheckResult{}, errors.New("check sei: deadline exceeded"), http.StatusBadGateway, []string{"The monitor did not answer. The hub's log has the details."}},
	}

	for _, a := range answers {
		monitored.check, monitored.checkErr = a.result, a.err
		code, body := check(asHTMX)
		if code != a.status || strings.Contains(body, "<html") {
			t.Errorf("a check %s with htmx = %d %q, want %d with a fragment", a.name, code, body, a.status)
		}
		for _, want := range a.want {
			if !strings.Contains(body, want) {
				t.Errorf("a check %s with htmx says %q, want %q", a.name, body, want)
			}
		}
	}
	if !strings.Contains(logs.String(), "the monitor did not answer the check") || !strings.Contains(logs.String(), "deadline exceeded") ||
		!strings.Contains(logs.String(), "monitor=sei") {
		t.Errorf("the failed check was not logged with its cause and the monitor:\n%s", logs.String())
	}

	monitored.check, monitored.checkErr = monitors.CheckResult{Accepted: true, Message: "queued"}, nil
	rec := serve(t.Context(), mux, http.MethodPost, "/settings/sei/check", "", fromHub, withCookie(cookie("admin")))
	result := inner(rec.Body.String(), "check-result")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<html") || !strings.Contains(result, "The monitor started a check.") ||
		startTag(rec.Body.String(), "editor") == "" {
		t.Errorf("a check without htmx = %d with the check result %q, want 200 with the monitor page and the result in #check-result", rec.Code, result)
	}

	viewer := serve(t.Context(), mux, http.MethodPost, "/settings/sei/check", "", fromHub, withCookie(cookie("viewer")))
	if viewer.Code != http.StatusOK || !strings.Contains(inner(viewer.Body.String(), "check-result"), "The monitor started a check.") {
		t.Errorf("a check by sei-viewer without htmx = %d %s, want 200 with the result in #check-result", viewer.Code, shown(t, viewer))
	}

	unknown := serve(t.Context(), mux, http.MethodPost, "/settings/nope/check", "", fromHub, withCookie(cookie("admin")))
	if unknown.Code != http.StatusNotFound {
		t.Errorf("a check of a monitor out of the catalog = %d, want 404", unknown.Code)
	}
}
