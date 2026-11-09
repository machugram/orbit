package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/machugram/orbit/internal/vault"
	"github.com/machugram/orbit/internal/vm"
)

func registerResources(server *mcp.Server, d *Deps) {
	server.AddResource(&mcp.Resource{
		URI:         "orbit://info",
		Name:        "info",
		MIMEType:    "application/json",
		Description: "Server version, platform, vault path, and audit path.",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		version := d.Version
		if version == "" {
			version = "dev"
		}
		return resourceJSON(req.Params.URI, map[string]any{
			"name":       "orbit",
			"version":    version,
			"goos":       runtime.GOOS,
			"goarch":     runtime.GOARCH,
			"vault_path": d.VaultPath,
			"audit_path": d.AuditPath,
		})
	})

	server.AddResource(&mcp.Resource{
		URI:         "orbit://doctor",
		Name:        "doctor",
		MIMEType:    "application/json",
		Description: "Platform, orbctl, docker, and vault/audit directory readiness.",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return resourceJSON(req.Params.URI, doctor(ctx, d))
	})

	server.AddResource(&mcp.Resource{
		URI:         "orbit://workspaces",
		Name:        "workspaces",
		MIMEType:    "application/json",
		Description: "Live workspaces. Warm bases are omitted. Use the name as a run or file target.",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		body, err := d.workspaces(ctx)
		if err != nil {
			return nil, err
		}
		return resourceJSON(req.Params.URI, body)
	})

	server.AddResource(&mcp.Resource{
		URI:         "orbit://secrets",
		Name:        "secrets",
		MIMEType:    "application/json",
		Description: "Secret names. Values are never included.",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		names, err := d.secretNames()
		if err != nil {
			return nil, err
		}
		return resourceJSON(req.Params.URI, map[string]any{"secrets": names})
	})
}

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type doctorResult struct {
	OK     bool          `json:"ok"`
	Checks []doctorCheck `json:"checks"`
}

func doctor(ctx context.Context, d *Deps) doctorResult {
	res := doctorResult{OK: true}
	add := func(name string, ok bool, detail string) {
		res.Checks = append(res.Checks, doctorCheck{Name: name, OK: ok, Detail: detail})
		if !ok {
			res.OK = false
		}
	}
	add("platform", runtime.GOOS == "darwin", runtime.GOOS+"/"+runtime.GOARCH)
	checkCommand(ctx, "orbctl", []string{"status"}, add)
	checkCommand(ctx, "docker", []string{"ps"}, add)
	checkWritableDir("vault_dir", filepath.Dir(d.VaultPath), add)
	checkWritableDir("audit_dir", filepath.Dir(d.AuditPath), add)
	return res
}

func checkCommand(ctx context.Context, name string, args []string, add func(string, bool, string)) {
	path, err := exec.LookPath(name)
	if err != nil {
		add(name, false, "not found on PATH")
		return
	}
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		detail = path
	}
	add(name, err == nil, detail)
}

func checkWritableDir(name, dir string, add func(string, bool, string)) {
	if dir == "" || dir == "." {
		add(name, false, "path is not configured")
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		add(name, false, err.Error())
		return
	}
	f, err := os.CreateTemp(dir, ".orbit-doctor-*")
	if err != nil {
		add(name, false, err.Error())
		return
	}
	path := f.Name()
	_ = f.Close()
	_ = os.Remove(path)
	add(name, true, dir)
}

type workspaceEntry struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	Distro  string `json:"distro,omitempty"`
	Image   string `json:"image,omitempty"`
	Host    string `json:"host,omitempty"`
	Carrier string `json:"carrier,omitempty"`
}

type workspaceList struct {
	Workspaces     []workspaceEntry `json:"workspaces"`
	ContainerError string           `json:"container_error,omitempty"`
}

func (d *Deps) workspaces(ctx context.Context) (workspaceList, error) {
	var out workspaceList
	vms, err := d.VMs.List(ctx)
	if err != nil {
		return workspaceList{}, err
	}
	for _, rec := range vms {
		if rec.State == vault.VMDestroyed || vm.IsBaseName(rec.ID) {
			continue
		}
		entry := workspaceEntry{Name: rec.ID, Kind: kindVM, State: string(rec.State), Distro: rec.Distro}
		if rec.State == vault.VMRunning {
			entry.Host = rec.ID + ".orb.local"
		}
		out.Workspaces = append(out.Workspaces, entry)
	}
	if d.MicroVMs != nil {
		mvs, err := d.MicroVMs.List()
		if err != nil {
			return workspaceList{}, err
		}
		for _, mv := range mvs {
			if mv.State == vault.VMDestroyed {
				continue
			}
			entry := workspaceEntry{
				Name: mv.ID, Kind: kindMicroVM, State: string(mv.State),
				Image: mv.Image, Carrier: mv.Carrier,
			}
			if mv.State == vault.VMRunning {
				entry.Host = mv.Carrier + ".orb.local"
			}
			out.Workspaces = append(out.Workspaces, entry)
		}
	}
	if d.Containers != nil {
		list, err := d.Containers.List(ctx, true)
		if err != nil {
			out.ContainerError = err.Error()
		} else {
			for _, c := range list {
				name := strings.TrimPrefix(c.Name, "/")
				out.Workspaces = append(out.Workspaces, workspaceEntry{
					Name: name, Kind: kindContainer, State: c.State, Image: c.Image,
					Host: name + ".orb.local",
				})
			}
		}
	}
	if out.Workspaces == nil {
		out.Workspaces = []workspaceEntry{}
	}
	return out, nil
}

func (d *Deps) secretNames() ([]string, error) {
	infos, err := d.Vault.ListCredentials("")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Key)
	}
	return names, nil
}
