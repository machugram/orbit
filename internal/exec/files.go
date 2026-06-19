package exec

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"

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
	client, sftpc, err := r.sftp(ctx, vmID)
	if err != nil {
		return err
	}
	defer client.Close()
	defer sftpc.Close()

	if dir := path.Dir(remotePath); dir != "." && dir != "/" {
		if err := sftpc.MkdirAll(dir); err != nil {
			return fmt.Errorf("exec: creating %s on %s: %w", dir, vmID, err)
		}
	}
	dst, err := sftpc.Create(remotePath)
	if err != nil {
		return fmt.Errorf("exec: creating %s on %s: %w", remotePath, vmID, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return fmt.Errorf("exec: uploading to %s: %w", vmID, err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("exec: uploading to %s: %w", vmID, err)
	}
	if err := sftpc.Chmod(remotePath, mode); err != nil {
		return fmt.Errorf("exec: chmod %s on %s: %w", remotePath, vmID, err)
	}
	return nil
}

// Pull downloads a file from the VM, capped at MaxPullSize.
func (r *Runner) Pull(ctx context.Context, vmID, remotePath string) ([]byte, error) {
	client, sftpc, err := r.sftp(ctx, vmID)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	defer sftpc.Close()

	f, err := sftpc.Open(remotePath)
	if err != nil {
		return nil, fmt.Errorf("exec: opening %s on %s: %w", remotePath, vmID, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, MaxPullSize+1))
	if err != nil {
		return nil, fmt.Errorf("exec: downloading %s from %s: %w", remotePath, vmID, err)
	}
	if len(data) > MaxPullSize {
		return nil, fmt.Errorf("exec: %s on %s exceeds the %d byte pull limit", remotePath, vmID, MaxPullSize)
	}
	return data, nil
}

func (r *Runner) sftp(ctx context.Context, vmID string) (closer interface{ Close() error }, c *sftp.Client, err error) {
	client, err := r.connect(ctx, vmID)
	if err != nil {
		return nil, nil, err
	}
	sftpc, err := sftp.NewClient(client)
	if err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("exec: starting sftp on %s: %w", vmID, err)
	}
	return client, sftpc, nil
}

func randToken() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
