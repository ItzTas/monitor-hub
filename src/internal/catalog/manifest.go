package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
)

const (
	protocolVersion = 2
	settingsLimit   = 1 << 20
)

var (
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	apkPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9+._-]*$`)
)

type Manifest struct {
	ProtocolVersion int      `json:"protocol_version"`
	Name            string   `json:"name"`
	Command         []string `json:"command"`
	Apk             []string `json:"apk"`
}

// Entry is one monitor directory. Settings is nil when the monitor has no settings.json or the
// hub ignored it; SettingsError says why it was ignored.
type Entry struct {
	Manifest      Manifest
	Dir           string
	Settings      *settings.Declaration
	SettingsError error
}

func ReadManifest(r io.Reader) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("unexpected data after the manifest object")
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// ManifestFile is the manifest name inside each monitor directory and image.
const ManifestFile = "monitor.json"

func ValidateName(name string) error {
	if !namePattern.MatchString(name) || strings.HasSuffix(name, "-") || strings.Contains(name, "--") {
		return fmt.Errorf("invalid monitor name %q", name)
	}
	return nil
}

func (m Manifest) Validate() error {
	if m.ProtocolVersion == 1 {
		return errors.New("protocol_version 1 is no longer supported: this hub speaks protocol_version 2, where the gRPC service is monitorhub.monitor.v1.Monitor; rebuild the monitor with the current SDK")
	}
	if m.ProtocolVersion != protocolVersion {
		return fmt.Errorf("unsupported protocol_version %d: this hub speaks protocol_version %d", m.ProtocolVersion, protocolVersion)
	}
	if err := ValidateName(m.Name); err != nil {
		return err
	}
	if len(m.Command) == 0 || m.Command[0] == "" {
		return errors.New("command must not be empty")
	}
	for _, pkg := range m.Apk {
		if !apkPattern.MatchString(pkg) {
			return fmt.Errorf("invalid apk package %q", pkg)
		}
	}
	return nil
}

func Load(root string) ([]Entry, error) {
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read monitors root: %w", err)
	}
	var entries []Entry
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		entry, err := loadEntry(filepath.Join(root, d.Name()))
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Manifest.Name < entries[j].Manifest.Name })
	return entries, nil
}

func loadEntry(dir string) (Entry, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Entry{}, fmt.Errorf("monitor directory %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(ManifestFile)
	if err != nil {
		return Entry{}, fmt.Errorf("monitor directory %s: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	m, err := ReadManifest(f)
	if err != nil {
		return Entry{}, fmt.Errorf("monitor directory %s: %w", dir, err)
	}
	if m.Name != filepath.Base(dir) {
		return Entry{}, fmt.Errorf("monitor directory %s: manifest name %q does not match directory", dir, m.Name)
	}
	declaration, settingsErr := readSettings(root)
	return Entry{Manifest: m, Dir: dir, Settings: declaration, SettingsError: settingsErr}, nil
}

func readSettings(root *os.Root) (*settings.Declaration, error) {
	// Without O_NONBLOCK, opening a FIFO would wait for a writer forever.
	f, err := root.OpenFile(settings.File, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Join(root.Name(), settings.File))
	}
	data, err := io.ReadAll(io.LimitReader(f, settingsLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > settingsLimit {
		return nil, errors.New(settings.File + " is larger than 1 MiB")
	}
	return settings.Read(data)
}
