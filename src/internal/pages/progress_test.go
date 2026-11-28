package pages

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
	"github.com/ItzTas/monitor-hub/src/internal/supervisor"
)

const (
	textUp       = "The monitor is running with the new values."
	textFailed   = "The monitor did not start."
	textRetrying = "The hub keeps the new values and keeps retrying at its usual intervals. Fix the values and save again."
)

func TestProgressSaysOnlyWhatTheSupervisorSaw(t *testing.T) {
	mux, configs, _ := newSaveSite(t, Deps{})
	progress := func(target string, options ...option) (int, string) {
		rec := serve(t.Context(), mux, http.MethodGet, target, "", append([]option{withCookie(cookie("admin"))}, options...)...)
		return rec.Code, rec.Body.String()
	}

	configs.progress = monitorconfig.Progress{State: monitorconfig.ProgressRestarting}
	if code, body := progress("/settings/sei/progress?shown=restarting", asHTMX); code != http.StatusNoContent || body != "" {
		t.Errorf("a restart still going, already shown = %d %q, want 204 with no body", code, body)
	}
	code, body := progress("/settings/sei/progress", asHTMX)
	if code != http.StatusOK || !strings.Contains(body, textRestarting) || !strings.Contains(body, `hx-get="/settings/sei/progress?shown=restarting"`) {
		t.Errorf("a restart going, not yet shown = %d %q, want 200 with the element that polls", code, body)
	}
	if strings.Contains(body, "<html") || strings.Contains(body, "autofocus") || strings.Contains(body, "hx-swap-oob") {
		t.Errorf("a restart going draws more than the outcome: %q", body)
	}

	configs.progress = monitorconfig.Progress{State: monitorconfig.ProgressUp, MonitorState: monitorpb.State_STATE_IDLE, Message: "ok <b>"}
	code, body = progress("/settings/sei/progress?shown=restarting", asHTMX)
	if code != http.StatusOK || !strings.Contains(body, textUp) || !strings.Contains(body, ">Idle<") || !strings.Contains(body, "ok &lt;b&gt;") {
		t.Errorf("a monitor up = %d %q, want 200 with %q, Idle and its message", code, body, textUp)
	}
	if strings.Contains(body, "hx-trigger") || strings.Contains(body, "autofocus") {
		t.Errorf("a monitor up still polls or takes the focus: %q", body)
	}
	buttonsBack(t, "a monitor up", body)

	configs.progress = monitorconfig.Progress{State: monitorconfig.ProgressFailed, Reason: "invalid configuration:\n  - EVENTS is wrong"}
	code, body = progress("/settings/sei/progress?shown=restarting", asHTMX)
	if code != http.StatusOK || !strings.Contains(body, textFailed) || !strings.Contains(body, textRetrying) ||
		!strings.Contains(body, `<p class="pre-line">invalid configuration:`+"\n  - EVENTS is wrong</p>") {
		t.Errorf("a monitor that did not start = %d %q, want 200 with the reason in a pre-line", code, body)
	}
	if strings.Contains(body, textUp) || strings.Contains(body, "hx-trigger") || !hasAttr(startTag(body, "save"), "hx-swap-oob", "true") {
		t.Errorf("a monitor that did not start says it runs, still polls or keeps the save button held: %q", body)
	}

	configs.progress = monitorconfig.Progress{State: monitorconfig.ProgressNoAnswer, Phase: supervisor.PhaseStarting}
	code, body = progress("/settings/sei/progress?shown=restarting", asHTMX)
	if code != http.StatusOK || !strings.Contains(body, "The monitor did not answer within 75 seconds. Last known phase: Starting.") ||
		!hasAttr(startTag(body, "save"), "hx-swap-oob", "true") {
		t.Errorf("a monitor with no answer = %d %q, want 200 with the phase Starting and the save button back", code, body)
	}

	configs.progress = monitorconfig.Progress{State: monitorconfig.ProgressNone}
	code, body = progress("/settings/sei/progress?shown=restarting", asHTMX)
	if code != http.StatusOK || strings.Contains(body, "hx-trigger") || strings.Contains(body, "<p") {
		t.Errorf("no restart known = %d %q, want 200 with no text and no poll", code, body)
	}
	buttonsBack(t, "no restart known", body)

	configs.progress = monitorconfig.Progress{State: monitorconfig.ProgressFailed, Reason: "invalid configuration"}
	code, body = progress("/settings/sei/progress")
	if code != http.StatusOK || !strings.Contains(body, "<html") || !strings.Contains(inner(body, "outcome"), textFailed) ||
		!strings.Contains(body, "invalid configuration") || startTag(body, "status") == "" {
		t.Errorf("the progress without htmx = %d, want 200 with the whole monitor page and the failure in the outcome:\n%s", code, body)
	}

	configs.errs["Progress"] = errors.Join(monitorconfig.ErrDatabase, errors.New("connection refused"))
	code, body = progress("/settings/sei/progress?shown=restarting", asHTMX)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "database cannot be reached") || strings.Contains(body, "hx-trigger") {
		t.Errorf("the progress with the database down = %d %q, want 503 with the message and no poll", code, body)
	}
	buttonsBack(t, "the progress with the database down", body)
	configs.errs["Progress"] = monitorconfig.ErrUnknown
	code, body = progress("/settings/sei/progress?shown=restarting", asHTMX)
	if code != http.StatusNotFound {
		t.Errorf("the progress when Progress does not know sei = %d, want 404", code)
	}
	buttonsBack(t, "the progress when Progress does not know sei", body)
	if code, _ := progress("/settings/nope/progress", asHTMX); code != http.StatusNotFound {
		t.Errorf("the progress of a monitor out of the catalog = %d, want 404", code)
	}
}

// buttonsBack checks that an answer that ends the poll gives both save buttons back, enabled.
func buttonsBack(t *testing.T, answer, body string) {
	t.Helper()
	for _, id := range []string{"save", "save-default"} {
		tag := startTag(body, id)
		if _, disabled := attr(tag, "disabled"); tag == "" || disabled || !hasAttr(tag, "hx-swap-oob", "true") {
			t.Errorf("%s: %s is %q, want it enabled and swapped out of band", answer, id, tag)
		}
	}
}
