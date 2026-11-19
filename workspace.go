package gitops

import (
	"context"
	"io/fs"
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
