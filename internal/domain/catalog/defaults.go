package catalog

import (
	"fmt"
	"maps"
	"slices"
)

// WithDefaults leaves the caller's catalog untouched and rejects ambiguous routes.
func WithDefaults(input, defaults *Catalog) (*Catalog, error) {
	out := &Catalog{Models: map[string]*Model{}, Aliases: map[string]*Alias{}}
	maps.Copy(out.Models, input.Models)
	maps.Copy(out.Aliases, input.Aliases)
	if err := addDefaultModels(out, defaults); err != nil {
		return nil, err
	}
	if err := addDefaultAliases(out, defaults); err != nil {
		return nil, err
	}
	return out, nil
}
func addDefaultModels(out, defaults *Catalog) error {
	if defaults == nil {
		return nil
	}
	for id, m := range defaults.Models {
		if _, exists := out.Aliases[id]; exists {
			return fmt.Errorf("worker model %q conflicts with an alias", id)
		}
		if old, exists := out.Models[id]; exists {
			if old.API != m.API || old.Protocol != m.Protocol || old.BaseURL != m.BaseURL || old.NewConnectionPerRequest != m.NewConnectionPerRequest {
				return fmt.Errorf("worker model %q conflicts with catalog; remove its manual entry or match workers.json", id)
			}
			continue
		}
		if err := validateModel(m); err != nil {
			return err
		}
		out.Models[id] = m
	}
	return nil
}
func addDefaultAliases(out, defaults *Catalog) error {
	if defaults == nil {
		return nil
	}
	for name, a := range defaults.Aliases {
		if _, exists := out.Models[name]; exists {
			return fmt.Errorf("worker alias %q conflicts with a model", name)
		}
		if old, exists := out.Aliases[name]; exists && !slices.Equal(old.Chain, a.Chain) {
			return fmt.Errorf("worker alias %q conflicts with catalog; rename the existing alias", name)
		}
		for _, id := range a.Chain {
			if _, exists := out.Models[id]; !exists {
				return fmt.Errorf("worker alias %q references missing model %q", name, id)
			}
		}
		out.Aliases[name] = a
	}
	return nil
}
