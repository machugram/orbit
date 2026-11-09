package vm

import (
	"fmt"
	"strings"
)

const baseNamePrefix = "orbit-base-"

// BaseName returns the warm cache machine name for a distro
// (e.g. "ubuntu:24.04" → "orbit-base-ubuntu-24.04").
func BaseName(distro string) string {
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		case r == ':':
			return '-'
		default:
			return -1
		}
	}, distro)
	name := baseNamePrefix + sanitized
	if !nameRe.MatchString(name) {
		// Extremely long distro strings: truncate to nameRe max length.
		if len(name) > 63 {
			name = name[:63]
		}
	}
	return name
}

// IsBaseName reports whether id is a reserved warm-base machine.
func IsBaseName(id string) bool {
	return strings.HasPrefix(id, baseNamePrefix)
}

func rejectBaseDestroy(id string) error {
	if IsBaseName(id) {
		return fmt.Errorf("vm: refusing to destroy warm base %q (managed by Orbit)", id)
	}
	return nil
}
