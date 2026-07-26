package app

// manifest は gzip 圧縮した .json.gz で保存する。A2 の実測で、多ファイル
// リポジトリでは差分転送の大半が非圧縮 JSON の manifest だったため
// （測定記録リポジトリ win-vhdx-worktree の docs/bench-vs-alternatives.md）。旧形式の .json は読み側で後方互換に
// 対応し、既存履歴の移行は不要。

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"

	"github.com/Hirano-Takaaki/varve/internal/store"
)

func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipBytes(encoded []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(zr)
	closeErr := zr.Close()
	if err == nil {
		err = closeErr
	}
	return raw, err
}

func putManifest(ctx context.Context, remote BlobStore, name, id string, manifestJSON []byte) error {
	encoded, err := gzipBytes(manifestJSON)
	if err != nil {
		return err
	}
	return remote.Put(ctx, remote.Key("snapshots", name, id+".json.gz"), encoded, "application/gzip")
}

// getManifestBytes は .json.gz を優先して取得し、無ければ旧形式の
// .json に落ちる。どちらも無い場合は旧形式側の NotFound エラーを返す
// （呼び出し側の isNotFound 判定を成立させるため）。
func getManifestBytes(ctx context.Context, remote BlobStore, name, id string) ([]byte, error) {
	encoded, err := remote.Get(ctx, remote.Key("snapshots", name, id+".json.gz"))
	if err == nil {
		return gunzipBytes(encoded)
	}
	if !isNotFound(err) {
		return nil, err
	}
	return remote.Get(ctx, remote.Key("snapshots", name, id+".json"))
}
