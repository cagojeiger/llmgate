package consumers

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// nameRule constrains consumer names to a safe lowercased identifier shape so
// that audit grep / log fields / future API surfaces all behave predictably.
// First char must be alphanumeric to avoid being mistaken for a CLI flag.
var nameRule = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,` + fmt.Sprint(maxNameLen-1) + `}$`)

var aliasRule = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,127}$`)

func validateConsumer(c *Consumer) error {
	if c.Name == "" {
		return errors.New("name is required")
	}
	if !nameRule.MatchString(c.Name) {
		return fmt.Errorf("name %q must match %s", c.Name, nameRule.String())
	}
	if len(c.KeyHashes) == 0 {
		return fmt.Errorf("consumer %q: key_hashes must list at least one hash", c.Name)
	}
	seen := make(map[string]struct{}, len(c.KeyHashes))
	for _, h := range c.KeyHashes {
		if err := validateHash(h); err != nil {
			return fmt.Errorf("consumer %q: %w", c.Name, err)
		}
		if _, dup := seen[h]; dup {
			return fmt.Errorf("consumer %q: duplicate hash %s", c.Name, shortHash(h))
		}
		seen[h] = struct{}{}
	}
	seenAliases := make(map[string]struct{}, len(c.AllowedAliases))
	for _, alias := range c.AllowedAliases {
		if !aliasRule.MatchString(alias) {
			return fmt.Errorf("consumer %q: allowed_alias %q must match %s", c.Name, alias, aliasRule.String())
		}
		if _, dup := seenAliases[alias]; dup {
			return fmt.Errorf("consumer %q: duplicate allowed_alias %q", c.Name, alias)
		}
		seenAliases[alias] = struct{}{}
	}
	return nil
}

func validateHash(h string) error {
	if !strings.HasPrefix(h, hashPrefix) {
		return fmt.Errorf("hash %q must start with %q", h, hashPrefix)
	}
	digest := strings.TrimPrefix(h, hashPrefix)
	if len(digest) != hashHexLen {
		return fmt.Errorf("hash %q hex length is %d, want %d", h, len(digest), hashHexLen)
	}
	for _, r := range digest {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return fmt.Errorf("hash %q has non-lowercase-hex char %q", h, r)
		}
	}
	return nil
}
