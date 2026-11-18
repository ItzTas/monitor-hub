package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const protocolVersion = 1

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

type Entry struct {
	Manifest Manifest
	Dir      string
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

func ValidateName(name string) error {
	if !namePattern.MatchString(name) || strings.HasSuffix(name, "-") || strings.Contains(name, "--") {
		return fmt.Errorf("invalid plugin name %q", name)
	}
	return nil
}

func (m Manifest) Validate() error {
	if m.ProtocolVersion != protocolVersion {
		return fmt.Errorf("unsupported protocol_version %d", m.ProtocolVersion)
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
		return nil, fmt.Errorf("read plugins root: %w", err)
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
		return Entry{}, fmt.Errorf("plugin directory %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open("plugin.json")
	if err != nil {
		return Entry{}, fmt.Errorf("plugin directory %s: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	m, err := ReadManifest(f)
	if err != nil {
		return Entry{}, fmt.Errorf("plugin directory %s: %w", dir, err)
	}
	if m.Name != filepath.Base(dir) {
		return Entry{}, fmt.Errorf("plugin directory %s: manifest name %q does not match directory", dir, m.Name)
	}
	return Entry{Manifest: m, Dir: dir}, nil
}
