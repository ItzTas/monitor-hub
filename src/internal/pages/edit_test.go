package pages

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

const (
	textNothingSaved   = "Nothing is saved yet. Choose Save and restart to keep these changes."
	textTypeAgain      = "Secret values are never sent back to the page. Type them again."
	textNewDeclaration = "This monitor's settings changed since the page was loaded. The form now shows the current ones. The hub saved nothing."
)

func newEditSite(t *testing.T, deps Deps) (*http.ServeMux, *fakeSettings, monitorconfig.View) {
	t.Helper()
	deps.Entries = monitorEntries(t)
	mux, configs, _, _ := newTestSite(t, deps)
	configs.view = nestedScene().view(declarationFrom(t, "nested-settings.json"))
	return mux, configs, configs.view
}

// rulesForm is the form of view with the rules a, b and c, each with its own actions, and op.
func rulesForm(view monitorconfig.View, op string) url.Values {
	return url.Values{
		"revision": {"7"}, "declaration": {declarationKey(view)}, "op": {op},
		"n:rules": {"3"}, "n:labels": {"0"},
		"v:rules[0].process": {"a"}, "n:rules[0].actions": {"1"}, "v:rules[0].actions[0]": {"urgent"},
		"v:rules[1].process": {"b"}, "n:rules[1].actions": {"1"}, "v:rules[1].actions[0]": {"ignore"},
		"v:rules[2].process": {"c"}, "n:rules[2].actions": {"2"}, "v:rules[2].actions[0]": {"urgent"}, "v:rules[2].actions[1]": {"ignore"},
		"v:ratio": {"1.5"},
	}
}

func edit(t *testing.T, mux http.Handler, form url.Values, options ...option) *httptest.ResponseRecorder {
	t.Helper()
	return serve(t.Context(), mux, http.MethodPost, "/settings/sei/form", form.Encode(), append([]option{fromHub, withCookie(cookie("admin"))}, options...)...)
}

func selected(body, id string) []string {
	var values []string
	for _, c := range options(body, id) {
		if c.selected {
			values = append(values, c.value)
		}
	}
	return values
}

func TestListEditsSaveNothingAndKeepEveryValue(t *testing.T) {
	mux, configs, view := newEditSite(t, Deps{})
	htmx := withHeader("HX-Request", "true")

	removed := edit(t, mux, rulesForm(view, "remove:rules[1]"), htmx)
	body := removed.Body.String()
	if removed.Code != http.StatusOK || strings.Contains(body, "<html") || startTag(body, "editor") == "" {
		t.Fatalf("removing rules[1] with htmx = %d %s, want 200 with the editor alone", removed.Code, shown(t, removed))
	}
	for id, want := range map[string]string{"f-rules-0--process": "a", "f-rules-1--process": "c", "f-ratio": "1.5"} {
		if !hasAttr(startTag(body, id), "value", want) {
			t.Errorf("after removing rules[1], %s is %q, want %q", id, startTag(body, id), want)
		}
	}
	if got := selected(body, "f-rules-1--actions-1"); !slices.Equal(got, []string{"ignore"}) || !hasAttr(named(body, "n:rules"), "value", "2") {
		t.Errorf("after removing rules[1], the actions of c select %v and n:rules is %q, want ignore at rules[1].actions[1] and 2", got, named(body, "n:rules"))
	}
	if !strings.Contains(text(removed), textNothingSaved) {
		t.Errorf("a list edit does not say nothing is saved yet:\n%s", shown(t, removed))
	}

	moved := edit(t, mux, rulesForm(view, "up:rules[2]"), htmx)
	body = moved.Body.String()
	if !hasAttr(startTag(body, "f-rules-1--process"), "value", "c") || !hasAttr(startTag(body, "f-rules-2--process"), "value", "b") ||
		!slices.Equal(selected(body, "f-rules-1--actions-1"), []string{"ignore"}) || !hasAttr(named(body, "n:rules[1].actions"), "value", "2") {
		t.Errorf("moving rules[2] up does not put c, with both its actions, before b:\n%s", shown(t, moved))
	}

	added := edit(t, mux, rulesForm(view, "add:labels"), htmx)
	if label := startTag(added.Body.String(), "f-labels-0"); label == "" || !hasAttr(label, "value", "") {
		t.Errorf("adding to the empty labels draws f-labels-0 as %q, want an empty control", label)
	}

	page := edit(t, mux, rulesForm(view, "remove:rules[1]"))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "<html") || startTag(page.Body.String(), "status") == "" ||
		!hasAttr(startTag(page.Body.String(), "f-rules-1--process"), "value", "c") {
		t.Errorf("removing rules[1] without htmx = %d %s, want 200 with the whole monitor page", page.Code, shown(t, page))
	}
	if got := configs.callCount("Save"); got != 0 {
		t.Errorf("Save ran %d times for list edits, want 0", got)
	}
}

func TestAListEditDrawsAt1000ItemsInAll(t *testing.T) {
	mux, _, view := newEditSite(t, Deps{})
	form := nestedFlood(1000)
	form.Set("declaration", declarationKey(view))
	form.Set("op", "add:rules[0].actions")

	rec := edit(t, mux, form, withHeader("HX-Request", "true"))

	if rec.Code != http.StatusOK {
		t.Fatalf("a list edit with 1000 rules of 1000 actions each = %d, want 200", rec.Code)
	}
	if rows := count(rec.Body.String(), `\bvalue="remove:`); rows > maxItems {
		t.Errorf("the answer draws %d item rows, want at most %d", rows, maxItems)
	}
}

func TestAListEditDrawsAtMost100NewVariables(t *testing.T) {
	mux, _, view := newEditSite(t, Deps{})
	form := rulesForm(view, "add:labels")
	for i := range 1000 {
		form.Add("new_name", fmt.Sprintf("NEW_%d", i))
		form.Add("new_value", "v")
		form.Add("new_kind", "plain")
	}

	rec := edit(t, mux, form, withHeader("HX-Request", "true"))

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("a list edit with 1000 new variables = %d, want 200", rec.Code)
	}
	if rows := count(body, `name="new_name"`); rows != 100 {
		t.Errorf("the answer draws %d new variables, want the first 100", rows)
	}
	if !hasAttr(startTag(body, "new-99-name"), "value", "NEW_99") {
		t.Errorf("the last new variable drawn is %q, want NEW_99", startTag(body, "new-99-name"))
	}
}

func TestAListEditNeverEchoesATypedSecret(t *testing.T) {
	mux, _, view := newEditSite(t, Deps{})
	form := rulesForm(view, "add:labels")
	form.Set("s:account.token", "typed-secret-one")
	form.Set("s:secret_key", "typed-secret-two")
	secrets := map[string]string{"account.token": "typed-secret-one", "secret_key": "typed-secret-two"}

	for name, options := range map[string][]option{"with htmx": {withHeader("HX-Request", "true")}, "without htmx": nil} {
		rec := edit(t, mux, form, options...)
		if rec.Code != http.StatusOK || !strings.Contains(text(rec), textTypeAgain) {
			t.Errorf("a list edit %s with typed secrets = %d, want 200 saying they must be typed again", name, rec.Code)
		}
		for path, secret := range secrets {
			if strings.Contains(rec.Body.String(), secret) || inHeaders(rec, secret) {
				t.Errorf("the answer to a list edit %s carries the secret typed for %s", name, path)
			}
		}
	}
}

func TestAListEditOnAStaleDeclarationIs409(t *testing.T) {
	mux, configs, view := newEditSite(t, Deps{})
	older := monitorconfig.View{Declared: true, Declaration: declarationFrom(t, "diario-settings.json")}
	form := rulesForm(view, "remove:rules[1]")
	form.Set("declaration", declarationKey(older))

	rec := edit(t, mux, form, withHeader("HX-Request", "true"))

	body := rec.Body.String()
	if rec.Code != http.StatusConflict || !strings.Contains(text(rec), textNewDeclaration) || rec.Header().Get("HX-Retarget") != "" {
		t.Fatalf("a list edit on an older declaration = %d %s, want 409 with %q in the editor", rec.Code, shown(t, rec), textNewDeclaration)
	}
	if !hasAttr(named(body, "declaration"), "value", declarationKey(view)) || !hasAttr(named(body, "n:rules"), "value", "2") ||
		!hasAttr(startTag(body, "f-rules-1--process"), "value", "b") {
		t.Errorf("the editor after a stale list edit is not the current one, as Read gives it:\n%s", shown(t, rec))
	}
	if strings.Contains(text(rec), textNothingSaved) {
		t.Error("the editor after a stale list edit says the edit is waiting to be saved")
	}
	if got := configs.callCount("Save"); got != 0 {
		t.Errorf("Save ran %d times for a stale list edit, want 0", got)
	}
}

func TestAListEditNeedsTheOriginTheSessionAndConfigure(t *testing.T) {
	mux, configs, view := newEditSite(t, Deps{})
	body := rulesForm(view, "add:labels").Encode()
	refusals := []struct {
		name     string
		options  []option
		status   int
		message  string
		location string
	}{
		{"without an Origin", []option{withCookie(cookie("admin"))}, http.StatusForbidden, textOrigin, ""},
		{"from another site", []option{withHeader("Origin", "http://evil.example"), withCookie(cookie("admin"))}, http.StatusForbidden, textOrigin, ""},
		{"without an Origin nor a session", nil, http.StatusForbidden, textOrigin, ""},
		{"for sei-viewer without an Origin", []option{withCookie(cookie("viewer"))}, http.StatusForbidden, textOrigin, ""},
		{"without a session", []option{fromHub}, http.StatusSeeOther, "", "/settings/login"},
		{"with only a token", []option{fromHub, withHeader("Authorization", "Bearer mht_x")}, http.StatusSeeOther, "", "/settings/login"},
		{"for sei-viewer", []option{fromHub, withCookie(cookie("viewer"))}, http.StatusForbidden, textNoConfigure, ""},
	}

	for _, r := range refusals {
		rec := serve(t.Context(), mux, http.MethodPost, "/settings/sei/form", body, r.options...)
		if rec.Code != r.status || !strings.Contains(text(rec), r.message) || rec.Header().Get("Location") != r.location {
			t.Errorf("a list edit %s = %d to %q %s, want %d to %q with %q", r.name, rec.Code, rec.Header().Get("Location"), shown(t, rec), r.status, r.location, r.message)
		}
	}
	if reads, saves := configs.callCount("Read"), configs.callCount("Save"); reads != 0 || saves != 0 {
		t.Errorf("refused list edits ran Read %d and Save %d times, want 0", reads, saves)
	}
}

func TestAListEditThatCannotBeDoneIsRefused(t *testing.T) {
	logs := &lockedBuffer{}
	mux, configs, view := newEditSite(t, Deps{Log: slog.New(slog.NewTextHandler(logs, nil))})

	broken := serve(t.Context(), mux, http.MethodPost, "/settings/sei/form", "op=%zz", fromHub, withCookie(cookie("admin")))
	if broken.Code != http.StatusBadRequest || !strings.Contains(text(broken), textIncomplete) {
		t.Errorf("a list edit with a broken escape = %d %s, want 400 with %q", broken.Code, shown(t, broken), textIncomplete)
	}

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/settings/sei/form", strings.NewReader(rulesForm(view, "add:labels").Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	fromHub(r)
	r.AddCookie(cookie("admin"))
	large := httptest.NewRecorder()
	r.Body = http.MaxBytesReader(large, r.Body, 8)
	mux.ServeHTTP(large, r)
	if large.Code != http.StatusRequestEntityTooLarge || !strings.Contains(text(large), textTooLarge) {
		t.Errorf("a list edit over the body limit = %d %s, want 413 with %q", large.Code, shown(t, large), textTooLarge)
	}

	incomplete := rulesForm(view, "add:labels")
	incomplete.Del("declaration")
	if rec := edit(t, mux, incomplete); rec.Code != http.StatusBadRequest || !strings.Contains(text(rec), textIncomplete) {
		t.Errorf("a list edit without the declaration = %d %s, want 400 with %q", rec.Code, shown(t, rec), textIncomplete)
	}

	unknown := serve(t.Context(), mux, http.MethodPost, "/settings/nope/form", rulesForm(view, "add:labels").Encode(), fromHub, withCookie(cookie("admin")))
	if unknown.Code != http.StatusNotFound || !strings.Contains(text(unknown), textUnknown) {
		t.Errorf("a list edit of a monitor out of the catalog = %d %s, want 404 with %q", unknown.Code, shown(t, unknown), textUnknown)
	}
	readsBefore := configs.callCount("Read")

	configs.errs["Read"] = monitorconfig.ErrUnknown
	if rec := edit(t, mux, rulesForm(view, "add:labels")); rec.Code != http.StatusNotFound || !strings.Contains(text(rec), textUnknown) {
		t.Errorf("a list edit when Read does not know sei = %d %s, want 404 with %q", rec.Code, shown(t, rec), textUnknown)
	}
	configs.errs["Read"] = errors.Join(monitorconfig.ErrDatabase, errors.New("connection refused"))
	if rec := edit(t, mux, rulesForm(view, "add:labels")); rec.Code != http.StatusServiceUnavailable || !strings.Contains(text(rec), textDatabaseRead) {
		t.Errorf("a list edit when Read fails = %d %s, want 503 with %q", rec.Code, shown(t, rec), textDatabaseRead)
	}
	if !strings.Contains(logs.String(), "connection refused") {
		t.Errorf("the failed Read was not logged:\n%s", logs.String())
	}
	if readsBefore != 0 || configs.callCount("Save") != 0 {
		t.Errorf("before the monitor was known to exist Read ran %d times, and Save ran %d times in all, want 0", readsBefore, configs.callCount("Save"))
	}
}

func inHeaders(rec *httptest.ResponseRecorder, s string) bool {
	for _, values := range rec.Header() {
		if slices.ContainsFunc(values, func(v string) bool { return strings.Contains(v, s) }) {
			return true
		}
	}
	return false
}
