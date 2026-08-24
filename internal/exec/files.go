package exec

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/pkg/sftp"
)

// MaxPullSize caps file_pull downloads to keep tool results sane.
const MaxPullSize = 32 << 20 // 32 MiB

// Push uploads a local file to the VM at remotePath, creating parent
// directories as needed and preserving the local file's mode bits.
func (r *Runner) Push(ctx context.Context, vmID, localPath, remotePath string) error {
	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("exec: opening %s: %w", localPath, err)
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return fmt.Errorf("exec: stat %s: %w", localPath, err)
	}
	return r.push(ctx, vmID, src, remotePath, st.Mode().Perm())
}

// PushBytes uploads in-memory data to the VM at remotePath.
func (r *Runner) PushBytes(ctx context.Context, vmID string, data []byte, remotePath string, mode os.FileMode) error {
	return r.push(ctx, vmID, bytes.NewReader(data), remotePath, mode)
}

func (r *Runner) push(ctx context.Context, vmID string, src io.Reader, remotePath string, mode os.FileMode) error {
	client, release, err := r.acquire(ctx, vmID)
	if err != nil {
		return err
	}
	ok := true
	defer func() { release(ok) }()
	if err := r.pushOn(client, vmID, src, remotePath, mode); err != nil {
		ok = false
		return err
	}
	return nil
}

// Pull downloads a file from the VM, capped at MaxPullSize.
func (r *Runner) Pull(ctx context.Context, vmID, remotePath string) ([]byte, error) {
	client, release, err := r.acquire(ctx, vmID)
	if err != nil {
		return nil, err
	}
	ok := true
	defer func() { release(ok) }()

	sftpc, err := sftp.NewClient(client)
	if err != nil {
		ok = false
		return nil, fmt.Errorf("exec: starting sftp on %s: %w", vmID, err)
	}
	defer sftpc.Close()

	f, err := sftpc.Open(remotePath)
	if err != nil {
		ok = false
		return nil, fmt.Errorf("exec: opening %s on %s: %w", remotePath, vmID, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, MaxPullSize+1))
	if err != nil {
		ok = false
		return nil, fmt.Errorf("exec: downloading %s from %s: %w", remotePath, vmID, err)
	}
	if len(data) > MaxPullSize {
		return nil, fmt.Errorf("exec: %s on %s exceeds the %d byte pull limit", remotePath, vmID, MaxPullSize)
	}
	return data, nil
}

func randToken() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
