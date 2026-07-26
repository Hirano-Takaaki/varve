package app

// publish / restore は Publish-DevDriveImage.ps1 / Restore-DevDriveImage.ps1 の
// 手順を CLI 単体に取り込んだもの。順序を間違えるとデータを失うため、
// 「疎通確認 → フラッシュ → デタッチ → push/pull → 再アタッチ」を 1 コマンドで行う。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// vhdxOps はテストから差し替えるための OS 操作の束。
type vhdxOps interface {
	// Detach はアタッチ中ならフラッシュしてデタッチする。
	// 戻り値: アタッチされていたか、割り当てられていたドライブレター。
	Detach(ctx context.Context, path string) (bool, string, error)
	// Mount はマウントして割り当てレターを返す。
	Mount(ctx context.Context, path, letter string, trust bool) (string, error)
}

type platformVHDX struct{}

func (platformVHDX) Detach(ctx context.Context, path string) (bool, string, error) {
	return detachVHDX(ctx, path)
}

func (platformVHDX) Mount(ctx context.Context, path, letter string, trust bool) (string, error) {
	return mountVHDX(ctx, path, letter, trust)
}

var osVHDX vhdxOps = platformVHDX{}

type PublishOptions struct {
	Name, Path, DriveLetter string
	Codec                   string
	Concurrency             int
	ChunkSize               int64
	IgnoreLock              bool
	SkipRemount             bool
	Trust                   bool
	Progress                io.Writer
}

// Publish は VHDX を安全にデタッチして push し、元の状態に戻す。
// push の成否によらず、アタッチされていた VHDX は必ず再アタッチを試みる。
func Publish(ctx context.Context, remote BlobStore, o PublishOptions) (err error) {
	out := o.Progress
	if out == nil {
		out = io.Discard
	}
	path, absErr := filepath.Abs(o.Path)
	if absErr != nil {
		return absErr
	}
	if _, statErr := os.Stat(path); statErr != nil {
		return statErr
	}

	// デタッチしてから資格情報の設定漏れに気付くのは避ける。
	if _, listErr := remote.List(ctx, "refs/"); listErr != nil {
		return fmt.Errorf("S3 connectivity check failed (nothing was detached): %w", listErr)
	}

	fmt.Fprintln(out, "flushing and detaching ...")
	wasAttached, previousLetter, detachErr := osVHDX.Detach(ctx, path)
	if detachErr != nil {
		return detachErr
	}
	if !wasAttached {
		fmt.Fprintln(out, "not attached; publishing as-is")
	}
	remountLetter := previousLetter
	if o.DriveLetter != "" {
		remountLetter = o.DriveLetter
	}
	defer func() {
		// push の成否によらず必ず戻す。デタッチしたまま放置すると
		// そのマシンで作業ができなくなる。
		if !wasAttached || o.SkipRemount {
			return
		}
		fmt.Fprintln(out, "re-attaching ...")
		letter, mountErr := osVHDX.Mount(context.WithoutCancel(ctx), path, remountLetter, o.Trust)
		if mountErr != nil {
			fmt.Fprintf(out, "warning: re-attach failed: %v\n", mountErr)
			fmt.Fprintf(out, "re-attach manually: varve pull is not needed; mount the local file (e.g. scripts\\Mount-DevDriveVhdx.ps1)\n")
			if err == nil {
				// 発行自体は成功している。それでも環境が外れたままなのは
				// 異常なので、成功として扱わない。
				err = fmt.Errorf("published successfully, but re-attach failed: %w", mountErr)
			}
			return
		}
		fmt.Fprintf(out, "re-attached at %s\n", letter)
	}()

	return Push(ctx, remote, PushOptions{
		Name: o.Name, Source: path, Kind: "vhdx", Codec: o.Codec,
		Concurrency: o.Concurrency, ChunkSize: o.ChunkSize,
		IgnoreLock: o.IgnoreLock, Progress: o.Progress,
	})
}

type RestoreOptions struct {
	Reference, Path, DriveLetter, Seed, CacheDir string
	Concurrency                                  int
	Force                                        bool
	Trust                                        bool
	Progress                                     io.Writer
}

// Restore は VHDX を取得してマウントする。復元先が既にアタッチされていれば
// フラッシュしてデタッチしてから置き換え、取得に失敗した場合は既存ファイルを
// 再アタッチして元の状態に戻す（取得は staging に書いてから置き換えるため、
// 失敗しても既存ファイルは無傷）。
func Restore(ctx context.Context, remote BlobStore, o RestoreOptions) error {
	out := o.Progress
	if out == nil {
		out = io.Discard
	}
	path, absErr := filepath.Abs(o.Path)
	if absErr != nil {
		return absErr
	}
	_, statErr := os.Stat(path)
	exists := statErr == nil
	if exists && !o.Force {
		return fmt.Errorf("%s already exists; pass --force to update it (the existing file is scanned as a seed and only changed chunks are downloaded)", path)
	}

	if _, listErr := remote.List(ctx, "refs/"); listErr != nil {
		return fmt.Errorf("S3 connectivity check failed (nothing was detached): %w", listErr)
	}

	wasAttached := false
	if exists {
		fmt.Fprintln(out, "flushing and detaching the existing VHDX ...")
		var detachErr error
		wasAttached, _, detachErr = osVHDX.Detach(ctx, path)
		if detachErr != nil {
			return detachErr
		}
	}

	pullErr := Pull(ctx, remote, PullOptions{
		Reference: o.Reference, Destination: path,
		CacheDir: o.CacheDir, Seed: o.Seed,
		Concurrency: o.Concurrency, Force: o.Force,
		Mount: true, DriveLetter: o.DriveLetter, Trust: o.Trust,
		Progress: o.Progress,
	})
	if pullErr != nil && wasAttached {
		fmt.Fprintln(out, "restore failed; re-attaching the previous VHDX ...")
		if _, mountErr := osVHDX.Mount(context.WithoutCancel(ctx), path, o.DriveLetter, o.Trust); mountErr != nil {
			fmt.Fprintf(out, "warning: re-attach also failed: %v\n", mountErr)
			return errors.Join(pullErr, fmt.Errorf("re-attach the previous VHDX manually: %w", mountErr))
		}
	}
	return pullErr
}
