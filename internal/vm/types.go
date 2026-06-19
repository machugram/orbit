// Package vm manages OrbStack Linux machines for the agent: lifecycle via
// the orbctl CLI, cloud-init provisioning, and state tracking in the vault.
package vm

import (
	"context"
	"fmt"
	"regexp"
)

// CmdRunner executes an orbctl invocation (args exclude the binary name)
// and returns stdout and stderr. Injectable so tests run against a fake.
type CmdRunner func(ctx context.Context, args ...string) (stdout, stderr []byte, err error)

// CreateSpec describes a machine to create.
type CreateSpec struct {
	// Distro is an OrbStack distro, optionally versioned ("ubuntu",
	// "ubuntu:24.04"). Required.
	Distro string
	// Name is the machine name; generated ("agent-xxxxxxxx") if empty.
	Name string
	// Username for the default Linux user. Defaults to "agent".
	Username string
	// UserData is cloud-init user data passed via --user-data. Optional.
	UserData []byte
	// DisableIsolation opts out of OrbStack isolated mode. By default
	// machines are created with --isolated (no macOS file sharing or
	// integration), per the spec's security model.
	DisableIsolation bool
}

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	distroRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.:-]{0,62}$`)
	userRe   = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

func (s *CreateSpec) validate() error {
	if !distroRe.MatchString(s.Distro) {
		return fmt.Errorf("vm: invalid distro %q", s.Distro)
	}
	if s.Name != "" && !nameRe.MatchString(s.Name) {
		return fmt.Errorf("vm: invalid machine name %q", s.Name)
	}
	if s.Username != "" && !userRe.MatchString(s.Username) {
		return fmt.Errorf("vm: invalid username %q", s.Username)
	}
	return nil
}
