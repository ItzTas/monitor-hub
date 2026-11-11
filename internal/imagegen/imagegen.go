package imagegen

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ItzTas/monitor-hub/internal/catalog"
)

const (
	dockerTimeout    = 5 * time.Minute
	removeTimeout    = time.Minute
	maxArchiveBytes  = 1 << 20
	maxManifestBytes = 64 << 10
	maxStderrBytes   = 8 << 10
	readConcurrency  = 4
	manifestFile     = "plugin.json"
	baseApk          = "ca-certificates"
)

var (
	imagePattern     = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]+)?(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}|@sha256:[a-f0-9]{64})$`)
	containerPattern = regexp.MustCompile(`^[0-9a-f]{12,128}$`)
)

type PluginImage struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type ManifestReader interface {
	ReadManifest(ctx context.Context, image string) (catalog.Manifest, error)
}

type runFunc func(ctx context.Context, args ...string) ([]byte, error)

type DockerReader struct {
	run runFunc
}

func LoadPlugins(r io.Reader) ([]PluginImage, error) {
	var file struct {
		Plugins *[]PluginImage `json:"plugins"`
	}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("decode plugins: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the plugins object")
	}
	if file.Plugins == nil {
		return nil, errors.New(`plugins: the "plugins" list is required`)
	}
	return *file.Plugins, nil
}

func Generate(ctx context.Context, entries []PluginImage, reader ManifestReader) ([]byte, error) {
	if err := validateEntries(entries); err != nil {
		return nil, err
	}
	manifests := make([]catalog.Manifest, len(entries))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(readConcurrency)
	for i, entry := range entries {
		g.Go(func() (err error) {
			manifests[i], err = reader.ReadManifest(gctx, entry.Image)
			if err != nil {
				return fmt.Errorf("plugin %q: read the manifest of %s: %w", entry.Name, entry.Image, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return Render(entries, manifests)
}

func Render(entries []PluginImage, manifests []catalog.Manifest) ([]byte, error) {
	if len(entries) != len(manifests) {
		return nil, fmt.Errorf("got %d manifests for %d plugins", len(manifests), len(entries))
	}
	if err := validateEntries(entries); err != nil {
		return nil, err
	}
	packages := map[string]struct{}{}
	for i, entry := range entries {
		if err := checkManifest(entry, manifests[i]); err != nil {
			return nil, err
		}
		for _, pkg := range manifests[i].Apk {
			packages[pkg] = struct{}{}
		}
	}
	var b strings.Builder
	b.WriteString(buildStage)
	fmt.Fprintf(&b, runtimeStage, apkList(packages))
	for _, entry := range entries {
		fmt.Fprintf(&b, "COPY --from=%s /plugin/ /opt/monitor-hub/plugins/%s/\n", entry.Image, entry.Name)
	}
	if len(entries) > 0 {
		b.WriteString("RUN chmod -R a+rX /opt/monitor-hub/plugins\n")
	}
	b.WriteString(finalStage)
	return []byte(b.String()), nil
}

func validateEntries(entries []PluginImage) error {
	seen := map[string]bool{}
	for _, entry := range entries {
		if !imagePattern.MatchString(entry.Image) {
			return fmt.Errorf("plugin %q: invalid image reference %q", entry.Name, entry.Image)
		}
		if seen[entry.Name] {
			return fmt.Errorf("duplicate plugin name %q", entry.Name)
		}
		seen[entry.Name] = true
	}
	return nil
}

func checkManifest(entry PluginImage, m catalog.Manifest) error {
	if err := validateManifest(m); err != nil {
		return fmt.Errorf("plugin %q: invalid manifest: %w", entry.Name, err)
	}
	if m.Name != entry.Name {
		return fmt.Errorf("plugin %q: the manifest of %s is named %q", entry.Name, entry.Image, m.Name)
	}
	return nil
}

func validateManifest(m catalog.Manifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = catalog.ReadManifest(bytes.NewReader(data))
	return err
}

func apkList(packages map[string]struct{}) string {
	delete(packages, baseApk)
	names := make([]string, 0, len(packages))
	for pkg := range packages {
		names = append(names, pkg)
	}
	sort.Strings(names)
	return strings.Join(append([]string{baseApk}, names...), " ")
}

func FirstDifference(want, got []byte) string {
	if bytes.Equal(want, got) {
		return ""
	}
	wantLines := strings.Split(string(want), "\n")
	gotLines := strings.Split(string(got), "\n")
	for i := 0; i < max(len(wantLines), len(gotLines)); i++ {
		w, g := lineAt(wantLines, i), lineAt(gotLines, i)
		if w != g {
			return fmt.Sprintf("line %d:\n-%s\n+%s", i+1, w, g)
		}
	}
	return ""
}

func lineAt(lines []string, i int) string {
	if i >= len(lines) {
		return "(end of file)"
	}
	if len(lines[i]) > 200 {
		return lines[i][:200] + "..."
	}
	return lines[i]
}

func (d *DockerReader) ReadManifest(ctx context.Context, image string) (catalog.Manifest, error) {
	if !imagePattern.MatchString(image) {
		return catalog.Manifest{}, fmt.Errorf("invalid image reference %q", image)
	}
	run := d.run
	if run == nil {
		run = runDocker
	}
	id, err := createContainer(ctx, run, image)
	if err != nil {
		return catalog.Manifest{}, err
	}
	archive, copyErr := copyManifest(ctx, run, id)
	removeErr := removeContainer(ctx, run, id)
	if copyErr != nil {
		return catalog.Manifest{}, errors.Join(copyErr, removeErr)
	}
	m, err := manifestFromArchive(archive)
	if err != nil {
		return catalog.Manifest{}, errors.Join(fmt.Errorf("read %s from %s: %w", manifestFile, image, err), removeErr)
	}
	return m, removeErr
}

func createContainer(ctx context.Context, run runFunc, image string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	out, err := run(ctx, "create", image)
	if err != nil {
		return "", fmt.Errorf("docker create %s: %w", image, err)
	}
	id := strings.TrimSpace(string(out))
	if !containerPattern.MatchString(id) {
		return "", fmt.Errorf("docker create %s: unexpected container id %q", image, id)
	}
	return id, nil
}

func copyManifest(ctx context.Context, run runFunc, id string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	out, err := run(ctx, "cp", id+":/plugin/"+manifestFile, "-")
	if err != nil {
		return nil, fmt.Errorf("docker cp: %w", err)
	}
	return out, nil
}

func removeContainer(ctx context.Context, run runFunc, id string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
	defer cancel()
	if _, err := run(ctx, "rm", id); err != nil {
		return fmt.Errorf("docker rm %s: %w", id, err)
	}
	return nil
}

func manifestFromArchive(archive []byte) (catalog.Manifest, error) {
	tr := tar.NewReader(bytes.NewReader(archive))
	header, err := tr.Next()
	if errors.Is(err, io.EOF) {
		return catalog.Manifest{}, errors.New("the archive is empty")
	}
	if err != nil {
		return catalog.Manifest{}, fmt.Errorf("read the archive: %w", err)
	}
	if header.Typeflag != tar.TypeReg || header.Name != manifestFile {
		return catalog.Manifest{}, fmt.Errorf("the archive starts with %q, not a regular file named %s", header.Name, manifestFile)
	}
	if header.Size > maxManifestBytes {
		return catalog.Manifest{}, fmt.Errorf("the manifest is %d bytes, over the %d byte limit", header.Size, maxManifestBytes)
	}
	return catalog.ReadManifest(io.LimitReader(tr, maxManifestBytes))
}

type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.limit {
		return 0, fmt.Errorf("output is over the %d byte limit", c.limit)
	}
	return c.buf.Write(p)
}

func runDocker(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	stdout := &cappedBuffer{limit: maxArchiveBytes}
	stderr := &cappedBuffer{limit: maxStderrBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.buf.String()))
	}
	return stdout.buf.Bytes(), nil
}

const buildStage = `# Code generated by cmd/imagegen from plugins.json. DO NOT EDIT.
# Regenerate with: go run ./cmd/imagegen

FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine3.24 AS build

WORKDIR /src

COPY go.mod go.sum ./
COPY sdk/go.mod sdk/go.sum sdk/
RUN go mod download

COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -trimpath \
      -ldflags "-s -w" \
      -o /out/monitor-hub .

`

const runtimeStage = `FROM alpine:3.24

# hadolint ignore=DL3018
RUN apk add --no-cache %s \
    && addgroup -S -g 65532 monitor-hub \
    && adduser -S -D -H -u 65532 -G monitor-hub monitor-hub \
    && mkdir -p /opt/monitor-hub/plugins /data \
    && chown 65532:65532 /data

COPY --from=build /out/monitor-hub /usr/local/bin/monitor-hub
`

const finalStage = `
USER 65532:65532

VOLUME ["/data"]

EXPOSE 8080

HEALTHCHECK --interval=1m --timeout=10s --start-period=5m --retries=3 \
    CMD ["sh", "-c", "address=\"${HTTP_ADDRESS-:8080}\"; [ -z \"$address\" ] || wget -q -O /dev/null \"http://127.0.0.1:${address##*:}/healthz\""]

ENTRYPOINT ["/usr/local/bin/monitor-hub"]
CMD ["serve"]
`
