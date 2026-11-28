package pages

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

const (
	textRestarting      = "Saved. The monitor is restarting."
	textChanged         = "Saved. The monitor's environment did not change, so the hub did not restart it."
	textNothingToSave   = "Nothing to save: no value changed."
	textReread          = "The hub saved the values but could not read them back. Reload the page."
	textRevision        = "The saved values changed in another tab or window. The form now shows them. The hub saved nothing, so make your changes again."
	textStillRestarting = "The monitor is still restarting after the last save. Wait until it settles, then save again. The hub saved nothing."
	textBusy            = "Another save of this monitor is in progress. The hub saved nothing."
	textDatabaseSave    = "The hub's database cannot be reached. The hub saved nothing."
	textNoDeclaration   = "This monitor does not declare its settings, so nothing can be changed here. Its values come from the hub's .env file."
	textTooLong         = "The lists in this configuration hold more than 1000 items in all, which is too many for this page. Change this field through the hub's API."
	textNoCheck         = "You do not have permission to ask this monitor for a check."
)

// newSaveSite serves the sei form of seiScene, with PROXY only in the .env.
func newSaveSite(t *testing.T, deps Deps) (*http.ServeMux, *fakeSettings, *fakeMonitors) {
	t.Helper()
	deps.Entries = monitorEntries(t)
	mux, configs, monitored, _ := newTestSite(t, deps)
	configs.view = seiScene().view(declarationFrom(t, "sei-settings.json"))
	configs.view.Variables = []monitorconfig.VariableView{{Name: "PROXY", Secret: true, Source: monitorconfig.SourceEnv, EnvSet: true}}
	return mux, configs, monitored
}

func save(t *testing.T, mux http.Handler, form url.Values, options ...option) *httptest.ResponseRecorder {
	t.Helper()
	return serve(t.Context(), mux, http.MethodPost, "/settings/sei", form.Encode(), append([]option{fromHub, withCookie(cookie("admin"))}, options...)...)
}

var asHTMX = withHeader("HX-Request", "true")

func TestSaveFromThePage(t *testing.T) {
	mux, configs, _ := newSaveSite(t, Deps{})
	form := formOf(t, configs.view)
	form.Set("v:sei_units", "12345")
	configs.saved = monitorconfig.SaveResult{Changed: []string{"sei_units"}, Restarted: true, Request: 1}
	configs.progress = monitorconfig.Progress{State: monitorconfig.ProgressRestarting}

	rec := save(t, mux, form, asHTMX)

	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(body, "<html") || startTag(body, "editor") == "" {
		t.Fatalf("a save with htmx = %d %s, want 200 with the editor alone", rec.Code, shown(t, rec))
	}
	revision := int64(7)
	want := []monitorconfig.Change{{Document: map[string]json.RawMessage{"sei_units": raw(`"12345"`)}, Revision: &revision}}
	configs.mu.Lock()
	got := slices.Clone(configs.changes)
	configs.mu.Unlock()
	if len(got) != 1 || !reflect.DeepEqual(got, want) {
		t.Fatalf("Save got %d changes, want one with only sei_units at revision 7", len(got))
	}
	poll := find(body, `<div\s[^>]*\bhx-get="/settings/sei/progress\?shown=restarting"[^>]*>`)
	if !hasAttr(poll, "hx-trigger", "every 1s") {
		t.Errorf("the outcome polls with %q, want every 1s", poll)
	}
	outcome := inner(body, "outcome")
	if !strings.Contains(outcome, textRestarting) || find(outcome, `<div tabindex="-1" autofocus>`) == "" {
		t.Errorf("the outcome is %q, want %q in an element that takes the focus", outcome, textRestarting)
	}
	for _, id := range []string{"save", "save-default"} {
		if _, disabled := attr(startTag(body, id), "disabled"); !disabled {
			t.Errorf("while the monitor restarts, %s is %q, want it disabled", id, startTag(body, id))
		}
	}
	formTag := find(body, `<form\s[^>]*\bhx-post="/settings/sei"[^>]*>`)
	if !hasAttr(formTag, "hx-disabled-elt", "#save, #save-default") {
		t.Errorf("the form is %q, want both save buttons disabled while a save runs", formTag)
	}
	if first := find(body, `<button\b[^>]*>`); !hasAttr(first, "id", "save-default") || strings.Contains(first, "name=") {
		t.Errorf("the first button of the form is %q, want save-default, which sends no op", first)
	}

	results := map[string]monitorconfig.SaveResult{
		textChanged:       {Changed: []string{"env:CMD"}},
		textNothingToSave: {},
	}
	configs.progress = monitorconfig.Progress{State: monitorconfig.ProgressNone}
	for message, result := range results {
		configs.saved = result
		rec := save(t, mux, formOf(t, configs.view), asHTMX)
		outcome := inner(rec.Body.String(), "outcome")
		if rec.Code != http.StatusOK || !strings.Contains(text(rec), message) || find(outcome, `<div tabindex="-1" autofocus>`) == "" {
			t.Errorf("a save that gives %v = %d with the outcome %q, want 200 with %q, focused", result, rec.Code, outcome, message)
		}
		if strings.Contains(rec.Body.String(), "hx-trigger") || strings.Contains(startTag(rec.Body.String(), "save"), "disabled") {
			t.Errorf("a save that gives %v polls or holds the save button", result)
		}
	}

	configs.saved = monitorconfig.SaveResult{Changed: []string{"sei_units"}, Restarted: true, Request: 2}
	configs.progress = monitorconfig.Progress{State: monitorconfig.ProgressUp}
	early := save(t, mux, form, asHTMX)
	for _, id := range []string{"save", "save-default"} {
		if _, disabled := attr(startTag(early.Body.String(), id), "disabled"); !disabled {
			t.Errorf("after a save that asked for a restart the progress does not show yet, %s is not disabled", id)
		}
	}

	page := save(t, mux, form)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "<html") || startTag(page.Body.String(), "status") == "" {
		t.Errorf("a save without htmx = %d %s, want 200 with the whole monitor page", page.Code, shown(t, page))
	}
}

func TestSaveErrorsFromThePage(t *testing.T) {
	cases := []struct {
		name     string
		set      func(*fakeSettings)
		status   int
		message  string
		retarget bool
		polls    bool
		logged   string
	}{
		{"the values have problems", func(f *fakeSettings) {
			f.errs["Save"] = &monitorconfig.ProblemsError{Problems: []monitorconfig.Problem{{Path: "sei_units", Message: "use digits"}}}
		}, http.StatusUnprocessableEntity, "use digits", false, false, ""},
		{"another save came first", func(f *fakeSettings) { f.errs["Save"] = monitorconfig.ErrStale }, http.StatusConflict, textRevision, false, false, ""},
		{"the monitor is restarting", func(f *fakeSettings) {
			f.errs["Save"] = monitorconfig.ErrRestarting
			f.onSave = func() { f.progress = monitorconfig.Progress{State: monitorconfig.ProgressRestarting} }
		}, http.StatusConflict, textStillRestarting, true, true, ""},
		{"the monitor is restarting and Progress fails", func(f *fakeSettings) {
			f.errs["Save"] = monitorconfig.ErrRestarting
			f.onSave = func() { f.errs["Progress"] = errors.New("database down") }
		}, http.StatusConflict, textStillRestarting, true, false, ""},
		{"another save runs", func(f *fakeSettings) { f.errs["Save"] = monitorconfig.ErrBusy }, http.StatusConflict, textBusy, true, false, ""},
		{"the validator fails", func(f *fakeSettings) {
			f.errs["Save"] = &monitorconfig.ValidatorError{Reason: "it exited with code 2", Stderr: "stderr of the validator"}
		}, http.StatusBadGateway, "The monitor could not check the values, so the hub saved nothing. Reason: it exited with code 2", true, false, ""},
		{"the database fails", func(f *fakeSettings) {
			f.errs["Save"] = errors.Join(monitorconfig.ErrDatabase, errors.New("connection refused"))
		}, http.StatusServiceUnavailable, textDatabaseSave, true, false, "connection refused"},
		{"Save does not know sei", func(f *fakeSettings) { f.errs["Save"] = monitorconfig.ErrUnknown }, http.StatusNotFound, textUnknown, true, false, ""},
		{"Save fails otherwise", func(f *fakeSettings) { f.errs["Save"] = errors.New("boom") }, http.StatusInternalServerError, textInternal, true, false, "boom"},
		{"Read fails", func(f *fakeSettings) {
			f.errs["Read"] = errors.Join(monitorconfig.ErrDatabase, errors.New("connection refused"))
		}, http.StatusServiceUnavailable, textDatabaseSave, true, false, "connection refused"},
		{"Read does not know sei", func(f *fakeSettings) { f.errs["Read"] = monitorconfig.ErrUnknown }, http.StatusNotFound, textUnknown, true, false, ""},
		{"the values cannot be read back", func(f *fakeSettings) {
			f.onSave = func() { f.errs["Read"] = errors.New("database down") }
		}, http.StatusServiceUnavailable, textReread, true, false, "database down"},
	}

	for _, c := range cases {
		logs := &lockedBuffer{}
		mux, configs, _ := newSaveSite(t, Deps{Log: slog.New(slog.NewTextHandler(logs, nil))})
		form := formOf(t, configs.view)
		form.Set("v:sei_units", "12345")
		c.set(configs)

		rec := save(t, mux, form, asHTMX)

		retargeted := rec.Header().Get("HX-Retarget") == "#outcome" && rec.Header().Get("HX-Reswap") == "innerHTML"
		if rec.Code != c.status || !strings.Contains(text(rec), c.message) || retargeted != c.retarget {
			t.Errorf("a save when %s = %d, HX-Retarget %q %s; want %d with %q, retargeted %v",
				c.name, rec.Code, rec.Header().Get("HX-Retarget"), shown(t, rec), c.status, c.message, c.retarget)
		}
		if polls := strings.Contains(rec.Body.String(), `hx-get="/settings/sei/progress?shown=restarting"`); polls != c.polls {
			t.Errorf("a save when %s polls the progress: %v, want %v", c.name, polls, c.polls)
		}
		if !c.retarget && startTag(rec.Body.String(), "editor") == "" {
			t.Errorf("a save when %s does not answer with the editor", c.name)
		}
		if c.logged != "" && (!strings.Contains(logs.String(), c.logged) || !strings.Contains(logs.String(), "monitor=sei")) {
			t.Errorf("a save when %s logged %q, want %q and the monitor", c.name, logs.String(), c.logged)
		}
		if strings.Contains(rec.Body.String(), "stderr of the validator") || strings.Contains(logs.String(), "stderr of the validator") {
			t.Errorf("a save when %s shows what the validator wrote to stderr", c.name)
		}
	}

	mux, configs, _ := newSaveSite(t, Deps{})
	configs.errs["Save"] = monitorconfig.ErrBusy
	page := save(t, mux, formOf(t, configs.view))
	if page.Code != http.StatusConflict || !strings.Contains(page.Body.String(), "<title>Not saved | ") || !strings.Contains(text(page), textBusy) {
		t.Errorf("a save without htmx while another runs = %d %s, want 409 with the page Not saved", page.Code, shown(t, page))
	}
}

func TestASaveThatCannotBeReadIsRefused(t *testing.T) {
	mux, configs, _ := newSaveSite(t, Deps{})
	form := formOf(t, configs.view)

	broken := serve(t.Context(), mux, http.MethodPost, "/settings/sei", "revision=%zz", fromHub, withCookie(cookie("admin")))
	if broken.Code != http.StatusBadRequest || !strings.Contains(text(broken), textIncomplete) {
		t.Errorf("a save with a broken escape = %d %s, want 400 with %q", broken.Code, shown(t, broken), textIncomplete)
	}

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/settings/sei", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	fromHub(r)
	r.AddCookie(cookie("admin"))
	large := httptest.NewRecorder()
	r.Body = http.MaxBytesReader(large, r.Body, 8)
	mux.ServeHTTP(large, r)
	if large.Code != http.StatusRequestEntityTooLarge || !strings.Contains(text(large), textTooLarge) {
		t.Errorf("a save over the body limit = %d %s, want 413 with %q", large.Code, shown(t, large), textTooLarge)
	}

	for _, key := range []string{"revision", "declaration"} {
		incomplete := formOf(t, configs.view)
		incomplete.Del(key)
		if rec := save(t, mux, incomplete); rec.Code != http.StatusBadRequest || !strings.Contains(text(rec), textIncomplete) {
			t.Errorf("a save without %s = %d %s, want 400 with %q", key, rec.Code, shown(t, rec), textIncomplete)
		}
	}
	if got := configs.callCount("Read"); got != 0 {
		t.Errorf("forms that cannot be read ran Read %d times, want 0", got)
	}
	notNumber := formOf(t, configs.view)
	notNumber.Set("revision", "abc")
	if rec := save(t, mux, notNumber); rec.Code != http.StatusBadRequest || !strings.Contains(text(rec), textIncomplete) {
		t.Errorf("a save with the revision abc = %d %s, want 400 with %q", rec.Code, shown(t, rec), textIncomplete)
	}

	unknown := serve(t.Context(), mux, http.MethodPost, "/settings/nope", form.Encode(), fromHub, withCookie(cookie("admin")))
	if unknown.Code != http.StatusNotFound || !strings.Contains(text(unknown), textUnknown) {
		t.Errorf("a save of a monitor out of the catalog = %d %s, want 404 with %q", unknown.Code, shown(t, unknown), textUnknown)
	}

	loud := formOf(t, configs.view)
	loud.Set("v:notification_detail", "loud")
	rec := save(t, mux, loud, asHTMX)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(text(rec), "Choose one of the options.") || startTag(rec.Body.String(), "error-notification_detail") == "" {
		t.Errorf("a save with a choice that is no option = %d %s, want 422 with the problem by its control", rec.Code, shown(t, rec))
	}
	if got := configs.callCount("Save"); got != 0 {
		t.Errorf("Save ran %d times for forms the page refused, want 0", got)
	}
}

func TestAClientThatLeftASaveGetsNothing(t *testing.T) {
	logs := &lockedBuffer{}
	mux, configs, _ := newSaveSite(t, Deps{Log: slog.New(slog.NewTextHandler(logs, nil))})
	configs.errs["Save"] = errors.Join(monitorconfig.ErrDatabase, errors.New("connection refused"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	rec := serve(ctx, mux, http.MethodPost, "/settings/sei", formOf(t, configs.view).Encode(), fromHub, withCookie(cookie("admin")))

	if !silent(rec) || strings.Contains(logs.String(), "could not save monitor configuration") {
		t.Errorf("a save for a client that left = %d %s with the log %q, want nothing", rec.Code, shown(t, rec), logs.String())
	}
}

func TestProblemsLandNextToTheirControls(t *testing.T) {
	mux, configs, _ := newSaveSite(t, Deps{})
	view := nestedScene().view(declarationFrom(t, "nested-settings.json"))
	proxy := "http://proxy.example"
	view.Variables = []monitorconfig.VariableView{{Name: "HTTP_PROXY", Source: monitorconfig.SourcePage, Value: &proxy, Saved: true}}
	view.Ignored = []monitorconfig.IgnoredView{{Path: "old_name", Value: raw(`1`), Reason: monitorconfig.ReasonUndeclared}}
	configs.view = view
	configs.errs["Save"] = &monitorconfig.ProblemsError{Problems: []monitorconfig.Problem{
		{Path: "rules[1].process", Message: "m1"}, {Path: "env:HTTP_PROXY", Message: "m2"}, {Path: "ignored:old_name", Message: "m3"},
		{Path: "", Message: "m4"}, {Path: "nope", Message: "m5"}, {Path: "rules", Message: "m6"}, {Path: "account", Message: "m7"},
	}}
	form := formOf(t, view)
	form.Set("v:ratio", "2")

	rec := save(t, mux, form, asHTMX)

	body := rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a save with problems = %d %s, want 422", rec.Code, shown(t, rec))
	}
	for id, want := range map[string]string{"error-rules-1--process": "m1", "error-var-HTTP_PROXY": "m2", "ignored-0": "m3", "error-rules": "m6", "error-account": "m7"} {
		if !strings.Contains(inner(body, id), want) {
			t.Errorf("%s holds %q, want %q", id, inner(body, id), want)
		}
	}
	summary := inner(body, "error-summary")
	for _, want := range []string{"m4", "nope: m5", `href="#f-rules-1--process"`, `href="#f-rules"`} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary has no %q:\n%s", want, summary)
		}
	}
	if element(startTag(body, "f-rules")) != "fieldset" {
		t.Errorf("the summary links to #f-rules, which is %q, want the fieldset of rules", startTag(body, "f-rules"))
	}
	if !hasAttr(startTag(body, "f-ratio"), "value", "2") {
		t.Errorf("f-ratio is %q, want the sent 2 back", startTag(body, "f-ratio"))
	}

	sc := scene{document: map[string]any{"watch_terms": []any{"PAULO SANTOS"}}, sources: map[string]monitorconfig.Source{"watch_terms": monitorconfig.SourcePage}, saved: []string{"watch_terms"}}
	configs.view = sc.view(declarationFrom(t, "diario-settings.json"))
	long := strings.TrimSpace(strings.Repeat("word ", 32))
	message := "WATCH_TERMS term 1 (32 words) is too long to match: shorten it"
	configs.errs["Save"] = &monitorconfig.ProblemsError{Problems: []monitorconfig.Problem{{Path: "watch_terms[0]", Message: message}}}
	terms := url.Values{
		"revision": {"7"}, "declaration": {declarationKey(configs.view)},
		"n:watch_terms": {"2"}, "v:watch_terms[0]": {""}, "v:watch_terms[1]": {long},
	}

	rec = save(t, mux, terms, asHTMX)

	body = rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(inner(body, "error-watch_terms-1"), message) || startTag(body, "error-watch_terms-0") != "" {
		t.Errorf("the problem of the saved watch_terms[0] = %d, error-watch_terms-1 %q, error-watch_terms-0 %q; want it by the second row of the form",
			rec.Code, inner(body, "error-watch_terms-1"), startTag(body, "error-watch_terms-0"))
	}
	if described, _ := attr(startTag(body, "f-watch_terms-1"), "aria-describedby"); !slices.Contains(strings.Fields(described), "error-watch_terms-1") {
		t.Errorf("f-watch_terms-1 is described by %q, want error-watch_terms-1", described)
	}
}

func TestAStaleFormIsRefusedAndRefreshed(t *testing.T) {
	more := *declarationFrom(t, "sei-settings.json")
	more.Fields = append(slices.Clone(more.Fields), settings.Field{Name: "sei_extra", Label: "Extra", Type: settings.String, Editable: true})
	cases := []struct {
		name    string
		reread  func(monitorconfig.View) monitorconfig.View
		message string
		check   func(t *testing.T, body string)
	}{
		{"another save moved the revision", func(v monitorconfig.View) monitorconfig.View { v.Revision = 8; return v }, textRevision, func(t *testing.T, body string) {
			if !hasAttr(named(body, "revision"), "value", "8") {
				t.Errorf("the refreshed form has the revision %q, want 8", named(body, "revision"))
			}
		}},
		{"the declaration changed", func(v monitorconfig.View) monitorconfig.View {
			decl := *v.Declaration
			decl.Fields = slices.Clone(decl.Fields)
			decl.Fields[0].Label = "SEI web address"
			v.Declaration = &decl
			return v
		}, textNewDeclaration, func(t *testing.T, body string) {
			if !strings.Contains(body, ">SEI web address") {
				t.Error("the refreshed form does not draw the current declaration")
			}
		}},
		{"the monitor no longer declares its settings", func(v monitorconfig.View) monitorconfig.View {
			return monitorconfig.View{Monitor: "sei", Revision: v.Revision}
		}, textNewDeclaration, func(t *testing.T, body string) {
			if !strings.Contains(body, textNoDeclaration) || strings.Contains(body, `name="v:`) {
				t.Error("the refreshed form of a monitor without a declaration draws values or does not say why")
			}
		}},
		{"the monitor declares one more field", func(v monitorconfig.View) monitorconfig.View { v.Declaration = &more; return v }, textNewDeclaration, func(t *testing.T, body string) {
			if startTag(body, "f-sei_extra") == "" {
				t.Error("the refreshed form has no control for the new field")
			}
		}},
	}

	for _, c := range cases {
		mux, configs, _ := newSaveSite(t, Deps{})
		form := formOf(t, configs.view)
		form.Set("v:sei_units", "12345")
		configs.view = c.reread(configs.view)

		rec := save(t, mux, form, asHTMX)

		if rec.Code != http.StatusConflict || !strings.Contains(text(rec), c.message) || startTag(rec.Body.String(), "editor") == "" {
			t.Errorf("a save when %s = %d %s, want 409 with the editor and %q", c.name, rec.Code, shown(t, rec), c.message)
		}
		c.check(t, text(rec))
		if got := configs.callCount("Save"); got != 0 {
			t.Errorf("Save ran %d times when %s, want 0", got, c.name)
		}
	}
}

func TestNoPageResponseCarriesATypedSecret(t *testing.T) {
	secrets := map[string]string{
		"sei_password": "typed-secret-one", "API_KEY": "typed-secret-two", "database_url": "typed-secret-three", "PROXY": "typed-secret-four",
	}
	outcomes := []struct {
		name string
		set  func(*fakeSettings, url.Values)
	}{
		{"a save", func(f *fakeSettings, _ url.Values) {
			f.saved = monitorconfig.SaveResult{Changed: []string{"sei_password"}, Restarted: true, Request: 1}
		}},
		{"problems", func(f *fakeSettings, _ url.Values) {
			f.errs["Save"] = &monitorconfig.ProblemsError{Problems: []monitorconfig.Problem{
				{Path: "sei_password", Message: "set exactly one password"}, {Path: "database_url", Message: "use a postgres address"},
			}}
		}},
		{"a problem of the page", func(_ *fakeSettings, form url.Values) { form.Set("v:notification_detail", "loud") }},
		{"a validator failure", func(f *fakeSettings, _ url.Values) {
			f.errs["Save"] = &monitorconfig.ValidatorError{Reason: "it exited with code 2"}
		}},
		{"an older revision", func(f *fakeSettings, _ url.Values) { f.view.Revision = 8 }},
		{"a new declaration", func(f *fakeSettings, _ url.Values) {
			decl := *f.view.Declaration
			decl.Fields = decl.Fields[1:]
			f.view.Declaration = &decl
		}},
		{"a restart", func(f *fakeSettings, _ url.Values) { f.errs["Save"] = monitorconfig.ErrRestarting }},
		{"another save", func(f *fakeSettings, _ url.Values) { f.errs["Save"] = monitorconfig.ErrBusy }},
		{"a database failure", func(f *fakeSettings, _ url.Values) { f.errs["Save"] = monitorconfig.ErrDatabase }},
		{"another failure", func(f *fakeSettings, _ url.Values) { f.errs["Save"] = errors.New("boom") }},
	}

	for _, o := range outcomes {
		for way, options := range map[string][]option{"with htmx": {asHTMX}, "without htmx": nil} {
			logs := &lockedBuffer{}
			mux, configs, _ := newSaveSite(t, Deps{Log: slog.New(slog.NewTextHandler(logs, nil))})
			form := formOf(t, configs.view)
			form.Set("s:sei_password", secrets["sei_password"])
			form.Set("s:database_url", secrets["database_url"])
			form.Set("var.PROXY", secrets["PROXY"])
			form["new_name"], form["new_value"], form["new_kind"] = []string{"API_KEY"}, []string{secrets["API_KEY"]}, []string{"secret"}
			o.set(configs, form)

			rec := save(t, mux, form, options...)

			for label, secret := range secrets {
				if strings.Contains(rec.Body.String(), secret) || inHeaders(rec, secret) || strings.Contains(logs.String(), secret) {
					t.Errorf("after %s %s, the answer or the log carries the secret typed for %s", o.name, way, label)
				}
			}
		}
	}
}

func TestEveryStateChangeNeedsTheSessionAndTheOrigin(t *testing.T) {
	mux, configs, monitored := newSaveSite(t, Deps{})
	body := formOf(t, configs.view).Encode()
	refusals := []struct {
		name     string
		options  []option
		status   int
		message  string
		location string
	}{
		{"without a session", []option{fromHub}, http.StatusSeeOther, "", "/settings/login"},
		{"with only a token", []option{fromHub, withHeader("Authorization", "Bearer mht_x")}, http.StatusSeeOther, "", "/settings/login"},
		{"without an Origin", []option{withCookie(cookie("admin"))}, http.StatusForbidden, textOrigin, ""},
		{"from another site", []option{withHeader("Origin", "http://evil.example"), withCookie(cookie("admin"))}, http.StatusForbidden, textOrigin, ""},
	}

	for _, target := range []string{"/settings/sei", "/settings/sei/form", "/settings/sei/check"} {
		for _, r := range refusals {
			rec := serve(t.Context(), mux, http.MethodPost, target, body, r.options...)
			if rec.Code != r.status || !strings.Contains(text(rec), r.message) || rec.Header().Get("Location") != r.location {
				t.Errorf("POST %s %s = %d to %q %s, want %d to %q with %q", target, r.name, rec.Code, rec.Header().Get("Location"), shown(t, rec), r.status, r.location, r.message)
			}
		}
		rec := serve(t.Context(), mux, http.MethodPost, target, body, fromHub, asHTMX)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("HX-Redirect") != "/settings/login" {
			t.Errorf("htmx POST %s without a session = %d with HX-Redirect %q, want 401 to /settings/login", target, rec.Code, rec.Header().Get("HX-Redirect"))
		}
	}
	if saves, checks := configs.callCount("Save"), monitored.callCount("Check"); saves != 0 || checks != 0 {
		t.Errorf("refused requests ran Save %d and Check %d times, want 0", saves, checks)
	}
}

func TestEveryRouteNeedsItsPermission(t *testing.T) {
	mux, configs, monitored := newSaveSite(t, Deps{})
	body := formOf(t, configs.view).Encode()
	refusals := []struct {
		session, method, target, message string
	}{
		{"viewer", http.MethodPost, "/settings/sei", textNoConfigure},
		{"viewer", http.MethodPost, "/settings/sei/form", textNoConfigure},
		{"viewer", http.MethodGet, "/settings/sei/progress", textNoConfigure},
		{"configurer", http.MethodPost, "/settings/sei/check", textNoCheck},
	}

	for _, r := range refusals {
		rec := serve(t.Context(), mux, r.method, r.target, body, fromHub, withCookie(cookie(r.session)))
		if rec.Code != http.StatusForbidden || !strings.Contains(text(rec), r.message) {
			t.Errorf("%s %s for %s = %d %s, want 403 with %q", r.method, r.target, r.session, rec.Code, shown(t, rec), r.message)
		}
	}
	for _, method := range []string{"Save", "Read", "Progress"} {
		if got := configs.callCount(method); got != 0 {
			t.Errorf("refused requests ran %s %d times, want 0", method, got)
		}
	}
	if got := monitored.callCount("Check"); got != 0 {
		t.Errorf("a refused check ran Check %d times, want 0", got)
	}

	monitored.check.Accepted = true
	rec := serve(t.Context(), mux, http.MethodPost, "/settings/sei/check", "", fromHub, withCookie(cookie("viewer")))
	if rec.Code != http.StatusOK || monitored.callCount("Check") != 1 {
		t.Errorf("a check by sei-viewer = %d %s with Check run %d times, want 200 and once", rec.Code, shown(t, rec), monitored.callCount("Check"))
	}
	if configs.callCount("Read") != 0 || strings.Contains(rec.Body.String(), `id="editor"`) {
		t.Error("the check page of sei-viewer, who may not configure sei, reads or draws its configuration")
	}
}

func TestListsTooLongToEditAreDrawnReadOnlyAndKept(t *testing.T) {
	mux, configs, _ := newSaveSite(t, Deps{})
	view := nestedScene().view(declarationFrom(t, "nested-settings.json"))
	view.Document["labels"] = longList(maxItems + 1)
	configs.view = view

	page := serve(t.Context(), mux, http.MethodGet, "/settings/sei", "", withCookie(cookie("admin")))
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(text(page), textTooLong) {
		t.Fatalf("the page of a configuration with 1001 labels = %d, want 200 with %q", page.Code, textTooLong)
	}
	for _, name := range []string{"n:labels", "n:rules", "v:labels[0]", "v:rules[0].process"} {
		if named(body, name) != "" {
			t.Errorf("with 1001 labels, the page draws the control %s", name)
		}
	}
	if named(body, "v:ratio") == "" {
		t.Error("with 1001 labels, the page draws no control for ratio")
	}

	form := drawnForm(body)
	form.Set("v:ratio", "2")
	rec := save(t, mux, form, asHTMX)

	configs.mu.Lock()
	changes := slices.Clone(configs.changes)
	configs.mu.Unlock()
	if rec.Code != http.StatusOK || len(changes) != 1 || !reflect.DeepEqual(changes[0].Document, map[string]json.RawMessage{"ratio": raw(`2`)}) {
		t.Errorf("a save from that page = %d with %d changes, want 200 and only ratio", rec.Code, len(changes))
	}
}
