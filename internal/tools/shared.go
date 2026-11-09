package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/machugram/orbit/internal/exec"
	"github.com/machugram/orbit/internal/microvm"
	"github.com/machugram/orbit/internal/vault"
	"github.com/machugram/orbit/internal/vm"
)

const (
	kindContainer = "container"
	kindVM        = "vm"
	kindMicroVM   = "microvm"

	defaultImage  = "alpine"
	defaultDistro = "ubuntu"
)

var workspaceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func newName(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func checkName(name string) error {
	if name == "" || !workspaceNameRe.MatchString(name) {
		return fmt.Errorf("invalid name %q (use lowercase letters, digits, '.', '_', '-')", name)
	}
	if vm.IsBaseName(name) {
		return fmt.Errorf("%q is a warm base image, not a workspace", name)
	}
	return nil
}

func withTimeout(ctx context.Context, seconds, def int) (context.Context, context.CancelFunc) {
	if seconds <= 0 {
		seconds = def
	}
	return context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func envNames(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	return names
}

func resolveExecSecretEnv(d *Deps, namespace string, secretEnv map[string]string) (map[string]string, error) {
	if len(secretEnv) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(secretEnv))
	for _, name := range envNames(secretEnv) {
		if !envNameRe.MatchString(name) {
			return nil, fmt.Errorf("invalid env var name %q", name)
		}
		val, err := d.Vault.GetCredential(namespace, secretEnv[name])
		if err != nil {
			return nil, fmt.Errorf("resolving secret %q for env %q: %w", secretEnv[name], name, err)
		}
		env[name] = string(val)
	}
	return env, nil
}

// findWorkspace reports the kind of an existing workspace. ok is false when
// nothing live has that name. A docker listing error is returned only when
// the name is not already a VM or microVM, so opening a machine does not
// depend on the container engine.
func (d *Deps) findWorkspace(ctx context.Context, name string) (kind string, ok bool, err error) {
	if err := checkName(name); err != nil {
		return "", false, err
	}
	if kind, ok, err = d.findMachine(name); ok || err != nil {
		return kind, ok, err
	}
	if d.Containers == nil {
		return "", false, nil
	}
	list, err := d.Containers.List(ctx, true)
	if err != nil {
		return "", false, err
	}
	for _, c := range list {
		if strings.TrimPrefix(c.Name, "/") == name {
			return kindContainer, true, nil
		}
	}
	return "", false, nil
}

// findMachine looks up a VM or microVM without calling docker.
func (d *Deps) findMachine(name string) (kind string, ok bool, err error) {
	rec, err := d.Vault.GetVM(name)
	if err != nil && !errors.Is(err, vault.ErrNotFound) {
		return "", false, err
	}
	if err == nil && rec.State != vault.VMDestroyed {
		return kindVM, true, nil
	}
	if d.MicroVMs == nil {
		return "", false, nil
	}
	mv, err := d.MicroVMs.Get(name)
	if err != nil && !errors.Is(err, vault.ErrNotFound) {
		return "", false, err
	}
	if err == nil && mv.State != vault.VMDestroyed {
		return kindMicroVM, true, nil
	}
	return "", false, nil
}

func missingWorkspace(name string) error {
	return fmt.Errorf("no workspace %q. Open one with workspace_open, or omit target on run for a throwaway container. orbit://workspaces lists what exists", name)
}

func (d *Deps) startWorkspace(ctx context.Context, name, kind string) error {
	switch kind {
	case kindVM:
		rec, err := d.Vault.GetVM(name)
		if err != nil {
			return err
		}
		if rec.State == vault.VMStopped {
			return d.VMs.Start(ctx, name)
		}
	case kindMicroVM:
		mv, err := d.MicroVMs.Get(name)
		if err != nil {
			return err
		}
		if mv.State == vault.VMStopped {
			_, err = d.MicroVMs.Start(ctx, name)
			return err
		}
	case kindContainer:
		list, err := d.Containers.List(ctx, true)
		if err != nil {
			return err
		}
		for _, c := range list {
			if strings.TrimPrefix(c.Name, "/") != name {
				continue
			}
			if c.State != "running" && !strings.HasPrefix(c.State, "Up") {
				return d.Containers.Start(ctx, name)
			}
		}
	}
	return nil
}

func (d *Deps) publicKey(kind, name string) string {
	var label string
	switch kind {
	case kindVM:
		label = vm.KeyLabel(name)
	case kindMicroVM:
		label = microvm.KeyLabel(name)
	default:
		return ""
	}
	pub, err := d.Keys.AuthorizedKey(label)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(pub)
}

func normalizeKind(kind string) (string, error) {
	switch kind {
	case "", kindContainer:
		return kindContainer, nil
	case kindVM, kindMicroVM:
		return kind, nil
	default:
		return "", fmt.Errorf("kind must be container, vm, or microvm")
	}
}

func checkInterpreter(name string) error {
	if !exec.SupportedInterpreter(name) {
		return fmt.Errorf("unsupported interpreter %q", name)
	}
	return nil
}
