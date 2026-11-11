package imagegen

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ItzTas/monitor-hub/internal/catalog"
)

const (
	seiImage    = "ghcr.io/itztas/sei-monitor:v2.0.0"
	diarioImage = "ghcr.io/itztas/diario-alerr:v2.0.0"
)

func manifest(name string, apk ...string) catalog.Manifest {
	return catalog.Manifest{ProtocolVersion: 1, Name: name, Command: []string{"./" + name}, Apk: apk}
}

func golden(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return string(data)
}

func TestRenderMatchesGolden(t *testing.T) {
	entries := []PluginImage{{Name: "sei", Image: seiImage}, {Name: "diario", Image: diarioImage}}
	got, err := Render(entries, []catalog.Manifest{manifest("sei"), manifest("diario", "poppler-utils")})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := golden(t, "Dockerfile.golden"); string(got) != want {
		t.Errorf("Render differs from the golden file:\n%s", FirstDifference([]byte(want), got))
	}
}

func TestRenderWithoutPluginsMatchesGolden(t *testing.T) {
	got, err := Render(nil, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := golden(t, "Dockerfile.empty.golden"); string(got) != want {
		t.Errorf("Render differs from the golden file:\n%s", FirstDifference([]byte(want), got))
	}
}

func TestRenderDedupesAndSortsApk(t *testing.T) {
	entries := []PluginImage{{Name: "sei", Image: seiImage}, {Name: "diario", Image: diarioImage}}
	manifests := []catalog.Manifest{
		manifest("sei", "poppler-utils", "ca-certificates", "tzdata"),
		manifest("diario", "poppler-utils", "curl"),
	}
	got, err := Render(entries, manifests)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	const want = "RUN apk add --no-cache ca-certificates curl poppler-utils tzdata \\\n"
	if !strings.Contains(string(got), want) {
		t.Errorf("Dockerfile has no line %q:\n%s", want, got)
	}
	if n := strings.Count(string(got), "poppler-utils"); n != 1 {
		t.Errorf("poppler-utils appears %d times, want 1", n)
	}
	if n := strings.Count(string(got), "ca-certificates"); n != 1 {
		t.Errorf("ca-certificates appears %d times, want 1", n)
	}
}

func TestManifestNameMustMatchEntry(t *testing.T) {
	_, err := Render([]PluginImage{{Name: "sei", Image: seiImage}}, []catalog.Manifest{manifest("diario")})
	if err == nil {
		t.Fatal("Render accepted a manifest whose name differs from the entry")
	}
	for _, part := range []string{`"sei"`, `"diario"`} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error %q does not mention %s", err, part)
		}
	}
}

func TestRenderRejectsInvalidInput(t *testing.T) {
	good := PluginImage{Name: "sei", Image: seiImage}
	digest := "ghcr.io/itztas/sei-monitor@sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name      string
		entries   []PluginImage
		manifests []catalog.Manifest
		want      string
	}{
		{"image with a space", []PluginImage{{Name: "sei", Image: "ghcr.io/itztas/sei-monitor:v1 --privileged"}}, []catalog.Manifest{manifest("sei")}, `"sei"`},
		{"image with a newline", []PluginImage{{Name: "sei", Image: "ghcr.io/itztas/sei-monitor:v1\nRUN id"}}, []catalog.Manifest{manifest("sei")}, `"sei"`},
		{"image with a command substitution", []PluginImage{{Name: "sei", Image: "ghcr.io/itztas/$(id):v1"}}, []catalog.Manifest{manifest("sei")}, `"sei"`},
		{"image with a semicolon", []PluginImage{{Name: "sei", Image: "ghcr.io/itztas/sei;id:v1"}}, []catalog.Manifest{manifest("sei")}, `"sei"`},
		{"image with uppercase", []PluginImage{{Name: "sei", Image: "ghcr.io/ItzTas/sei-monitor:v1"}}, []catalog.Manifest{manifest("sei")}, `"sei"`},
		{"image without a tag", []PluginImage{{Name: "sei", Image: "ghcr.io/itztas/sei-monitor"}}, []catalog.Manifest{manifest("sei")}, `"sei"`},
		{"image starting with a dash", []PluginImage{{Name: "sei", Image: "-ghcr.io/itztas/sei-monitor:v1"}}, []catalog.Manifest{manifest("sei")}, `"sei"`},
		{"image without a registry path", []PluginImage{{Name: "sei", Image: "sei-monitor:v1"}}, []catalog.Manifest{manifest("sei")}, `"sei"`},
		{"empty image", []PluginImage{{Name: "sei"}}, []catalog.Manifest{manifest("sei")}, `"sei"`},
		{"short digest", []PluginImage{{Name: "sei", Image: "ghcr.io/itztas/sei-monitor@sha256:abc"}}, []catalog.Manifest{manifest("sei")}, `"sei"`},
		{"duplicate names", []PluginImage{good, {Name: "sei", Image: digest}}, []catalog.Manifest{manifest("sei"), manifest("sei")}, `duplicate plugin name "sei"`},
		{"invalid apk package", []PluginImage{good}, []catalog.Manifest{manifest("sei", "curl && id")}, `"sei"`},
		{"invalid plugin name", []PluginImage{{Name: "Sei Monitor", Image: seiImage}}, []catalog.Manifest{manifest("Sei Monitor")}, `"Sei Monitor"`},
		{"fewer manifests than entries", []PluginImage{good}, nil, "manifests"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Render(tt.entries, tt.manifests)
			if err == nil {
				t.Fatalf("Render accepted the input:\n%s", out)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %s", err, tt.want)
			}
		})
	}
}

func TestRenderAcceptsDigestReference(t *testing.T) {
	image := "ghcr.io/itztas/sei-monitor@sha256:" + strings.Repeat("0123456789abcdef", 4)
	got, err := Render([]PluginImage{{Name: "sei", Image: image}}, []catalog.Manifest{manifest("sei")})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := "COPY --from=" + image + " /plugin/ /opt/monitor-hub/plugins/sei/\n"; !strings.Contains(string(got), want) {
		t.Errorf("Dockerfile has no line %q", want)
	}
}

type fakeReader struct {
	mu        sync.Mutex
	calls     []string
	manifests map[string]catalog.Manifest
	err       error
}

func (f *fakeReader) ReadManifest(_ context.Context, image string) (catalog.Manifest, error) {
	f.mu.Lock()
	f.calls = append(f.calls, image)
	f.mu.Unlock()
	return f.manifests[image], f.err
}

func TestGenerateReadsEveryImage(t *testing.T) {
	reader := &fakeReader{manifests: map[string]catalog.Manifest{seiImage: manifest("sei"), diarioImage: manifest("diario", "poppler-utils")}}
	entries := []PluginImage{{Name: "sei", Image: seiImage}, {Name: "diario", Image: diarioImage}}
	got, err := Generate(t.Context(), entries, reader)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	slices.Sort(reader.calls)
	if want := []string{diarioImage, seiImage}; !reflect.DeepEqual(reader.calls, want) {
		t.Errorf("images read = %v, want %v", reader.calls, want)
	}
	if want := golden(t, "Dockerfile.golden"); string(got) != want {
		t.Errorf("Generate differs from the golden file:\n%s", FirstDifference([]byte(want), got))
	}
}

func TestGenerateWithoutPluginsNeedsNoReader(t *testing.T) {
	got, err := Generate(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if want := golden(t, "Dockerfile.empty.golden"); string(got) != want {
		t.Errorf("Generate differs from the golden file:\n%s", FirstDifference([]byte(want), got))
	}
}

func TestGenerateNamesTheEntryWhoseImageFailed(t *testing.T) {
	reader := &fakeReader{err: errors.New("no such image")}
	_, err := Generate(t.Context(), []PluginImage{{Name: "sei", Image: seiImage}}, reader)
	if err == nil || !strings.Contains(err.Error(), `"sei"`) || !errors.Is(err, reader.err) {
		t.Errorf("Generate error = %v, want one naming the entry and wrapping the cause", err)
	}
}

func TestGenerateValidatesBeforeReading(t *testing.T) {
	reader := &fakeReader{}
	_, err := Generate(t.Context(), []PluginImage{{Name: "sei", Image: "ghcr.io/itztas/$(id):v1"}}, reader)
	if err == nil {
		t.Fatal("Generate accepted an invalid image")
	}
	if len(reader.calls) != 0 {
		t.Errorf("reader called with %v, want no calls", reader.calls)
	}
}

func TestLoadPlugins(t *testing.T) {
	got, err := LoadPlugins(strings.NewReader(`{"plugins":[{"name":"sei","image":"` + seiImage + `"}]}`))
	if err != nil {
		t.Fatalf("LoadPlugins: %v", err)
	}
	if want := []PluginImage{{Name: "sei", Image: seiImage}}; !reflect.DeepEqual(got, want) {
		t.Errorf("LoadPlugins = %v, want %v", got, want)
	}
	empty, err := LoadPlugins(strings.NewReader(`{"plugins":[]}`))
	if err != nil || len(empty) != 0 {
		t.Errorf("LoadPlugins(empty) = %v, %v, want no entries and no error", empty, err)
	}
}

func TestLoadPluginsRejectsMalformedInput(t *testing.T) {
	for name, input := range map[string]string{
		"unknown field":    `{"plugins":[],"extra":1}`,
		"unknown entry":    `{"plugins":[{"name":"sei","image":"` + seiImage + `","tag":"x"}]}`,
		"trailing data":    `{"plugins":[]} {}`,
		"not json":         `plugins`,
		"missing plugins":  `{}`,
		"null plugin list": `{"plugins":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadPlugins(strings.NewReader(input)); err == nil {
				t.Error("LoadPlugins accepted the input")
			}
		})
	}
}

func TestFirstDifference(t *testing.T) {
	got := FirstDifference([]byte("a\nb\nc\n"), []byte("a\nx\nc\n"))
	for _, part := range []string{"line 2", "-b", "+x"} {
		if !strings.Contains(got, part) {
			t.Errorf("FirstDifference = %q, want it to contain %q", got, part)
		}
	}
	if got := FirstDifference([]byte("a\n"), []byte("a\nb\n")); !strings.Contains(got, "line 2") {
		t.Errorf("FirstDifference for an added line = %q, want line 2", got)
	}
	if got := FirstDifference([]byte("a\n"), []byte("a\n")); got != "" {
		t.Errorf("FirstDifference for equal input = %q, want empty", got)
	}
}

const containerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type dockerCall struct {
	args        []string
	hasDeadline bool
	ctxErr      error
}

type fakeDocker struct {
	calls  []dockerCall
	create func() ([]byte, error)
	cp     func(cancel func()) ([]byte, error)
	cancel func()
}

func (f *fakeDocker) run(ctx context.Context, args ...string) ([]byte, error) {
	_, hasDeadline := ctx.Deadline()
	f.calls = append(f.calls, dockerCall{args: args, hasDeadline: hasDeadline, ctxErr: ctx.Err()})
	switch args[0] {
	case "create":
		if f.create != nil {
			return f.create()
		}
		return []byte(containerID + "\n"), nil
	case "cp":
		return f.cp(f.cancel)
	}
	return nil, nil
}

func (f *fakeDocker) verbs() []string {
	var verbs []string
	for _, c := range f.calls {
		verbs = append(verbs, c.args[0])
	}
	return verbs
}

func tarOf(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	return tarEntry(t, &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}, body)
}

func tarEntry(t *testing.T, header *tar.Header, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	if err := w.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const manifestJSON = `{"protocol_version":1,"name":"diario","command":["./diario-alerr"],"apk":["poppler-utils"]}`

func TestDockerReaderCreatesCopiesAndRemoves(t *testing.T) {
	docker := &fakeDocker{cp: func(func()) ([]byte, error) { return tarOf(t, "plugin.json", []byte(manifestJSON)), nil }}
	got, err := (&DockerReader{run: docker.run}).ReadManifest(t.Context(), diarioImage)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if want := (catalog.Manifest{ProtocolVersion: 1, Name: "diario", Command: []string{"./diario-alerr"}, Apk: []string{"poppler-utils"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("manifest = %+v, want %+v", got, want)
	}
	want := [][]string{
		{"create", diarioImage},
		{"cp", containerID + ":/plugin/plugin.json", "-"},
		{"rm", containerID},
	}
	for i, c := range docker.calls {
		if i >= len(want) || !reflect.DeepEqual(c.args, want[i]) {
			t.Fatalf("docker calls = %v, want %v", docker.verbs(), want)
		}
		if !c.hasDeadline {
			t.Errorf("docker %s ran without a time limit", c.args[0])
		}
	}
	if len(docker.calls) != len(want) {
		t.Errorf("docker calls = %v, want %v", docker.verbs(), want)
	}
}

func TestDockerReaderRemovesTheContainerOnEveryFailure(t *testing.T) {
	tests := map[string]func() ([]byte, error){
		"copy fails":       nil,
		"not a tar":        func() ([]byte, error) { return []byte("not a tar"), nil },
		"wrong file":       func() ([]byte, error) { return tarOf(t, "other.json", []byte(manifestJSON)), nil },
		"invalid manifest": func() ([]byte, error) { return tarOf(t, "plugin.json", []byte(`{"name":"x"}`)), nil },
		"oversized manifest": func() ([]byte, error) {
			return tarOf(t, "plugin.json", bytes.Repeat([]byte(" "), maxManifestBytes+1)), nil
		},
		"empty archive": func() ([]byte, error) { return nil, nil },
		"directory not a file": func() ([]byte, error) {
			return tarEntry(t, &tar.Header{Name: "plugin.json", Mode: 0o755, Typeflag: tar.TypeDir}, nil), nil
		},
	}
	for name, cp := range tests {
		t.Run(name, func(t *testing.T) {
			docker := &fakeDocker{cp: func(func()) ([]byte, error) { return nil, errors.New("boom") }}
			if cp != nil {
				docker.cp = func(func()) ([]byte, error) { return cp() }
			}
			if _, err := (&DockerReader{run: docker.run}).ReadManifest(t.Context(), diarioImage); err == nil {
				t.Fatal("ReadManifest succeeded")
			}
			if want := []string{"create", "cp", "rm"}; !reflect.DeepEqual(docker.verbs(), want) {
				t.Errorf("docker calls = %v, want %v", docker.verbs(), want)
			}
		})
	}
}

func TestDockerReaderRemovesTheContainerAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	docker := &fakeDocker{cancel: cancel, cp: func(cancel func()) ([]byte, error) {
		cancel()
		return nil, context.Canceled
	}}
	if _, err := (&DockerReader{run: docker.run}).ReadManifest(ctx, diarioImage); err == nil {
		t.Fatal("ReadManifest succeeded")
	}
	last := docker.calls[len(docker.calls)-1]
	if last.args[0] != "rm" || last.ctxErr != nil || !last.hasDeadline {
		t.Errorf("last call = %+v, want rm under a live context with a time limit", last)
	}
}

func TestDockerReaderStopsWhenCreateFails(t *testing.T) {
	docker := &fakeDocker{create: func() ([]byte, error) { return nil, errors.New("no such image") }}
	if _, err := (&DockerReader{run: docker.run}).ReadManifest(t.Context(), diarioImage); err == nil {
		t.Fatal("ReadManifest succeeded")
	}
	if want := []string{"create"}; !reflect.DeepEqual(docker.verbs(), want) {
		t.Errorf("docker calls = %v, want %v", docker.verbs(), want)
	}
}

func TestDockerReaderIgnoresAMalformedContainerID(t *testing.T) {
	docker := &fakeDocker{create: func() ([]byte, error) { return []byte("--force\n"), nil }}
	if _, err := (&DockerReader{run: docker.run}).ReadManifest(t.Context(), diarioImage); err == nil {
		t.Fatal("ReadManifest succeeded")
	}
	if want := []string{"create"}; !reflect.DeepEqual(docker.verbs(), want) {
		t.Errorf("docker calls = %v, want %v", docker.verbs(), want)
	}
}

func TestDockerReaderRejectsAnInvalidImageBeforeRunningDocker(t *testing.T) {
	docker := &fakeDocker{}
	if _, err := (&DockerReader{run: docker.run}).ReadManifest(t.Context(), "$(id)"); err == nil {
		t.Fatal("ReadManifest succeeded")
	}
	if len(docker.calls) != 0 {
		t.Errorf("docker calls = %v, want none", docker.verbs())
	}
}
