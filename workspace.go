package gitops

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

func checkWorkspaceSize(ctx context.Context, root string, maximum int64) error {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() > maximum-total {
			return ErrCloneSizeLimit
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func monitorWorkspaceSize(ctx context.Context, root string, maximum int64, cancel context.CancelFunc) error {
	if err := checkWorkspaceSize(ctx, root, maximum); err != nil {
		cancel()
		return err
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := checkWorkspaceSize(ctx, root, maximum); err != nil {
				cancel()
				return err
			}
		}
	}
}

func publishWorkspace(staging, destination string, replaceExisting bool) error {
	if !replaceExisting {
		return os.Rename(staging, destination)
	}
	parent := filepath.Dir(destination)
	backup, err := os.MkdirTemp(parent, "."+filepath.Base(destination)+".gitops-backup-")
	if err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	if err := os.Rename(destination, backup); err != nil {
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		if restoreErr := os.Rename(backup, destination); restoreErr != nil {
			return fmt.Errorf("publish staged workspace: %w; original workspace remains at %q: %v", err, backup, restoreErr)
		}
		return fmt.Errorf("publish staged workspace: %w", err)
	}
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("new workspace was published but backup %q could not be removed: %w", backup, err)
	}
	return nil
}
