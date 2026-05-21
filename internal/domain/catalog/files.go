package catalog

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// decodeStrict parses yaml with KnownFields(true) so any field not declared
// on the struct fails boot. Catches typos like 'protcol:' or stale fields
// like 'specs:' immediately rather than letting them silently no-op.
func decodeStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return err
	}
	return nil
}

// walkYAML iterates *.yaml / *.yml files in dir. Returns an error if the
// directory is missing — used for required dirs like models/.
func walkYAML(fsys fs.FS, dir string, fn func(name string, data []byte) error) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("catalog: read %s: %w", dir, err)
	}
	return walkEntries(fsys, dir, entries, fn)
}

// walkYAMLOptional is walkYAML but treats a missing dir as empty (so
// aliases/ being absent means "no aliases", not an error).
func walkYAMLOptional(fsys fs.FS, dir string, fn func(name string, data []byte) error) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("catalog: read %s: %w", dir, err)
	}
	return walkEntries(fsys, dir, entries, fn)
}

func walkEntries(fsys fs.FS, dir string, entries []fs.DirEntry, fn func(name string, data []byte) error) error {
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return fmt.Errorf("catalog: read %s/%s: %w", dir, name, err)
		}
		if err := fn(name, data); err != nil {
			return err
		}
	}
	return nil
}
