// Package config は remote 接続設定の永続化を担う。
// 資格情報はこのファイルに保存しない。AWS 標準環境変数から読む方針を維持する。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const FormatVersion = 1

type Remote struct {
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix,omitempty"`
	Region    string `json:"region,omitempty"`
	PathStyle *bool  `json:"path_style,omitempty"`
	Insecure  bool   `json:"insecure,omitempty"`
	// Compression は push の既定コーデック（gzip / zstd）。空は gzip。
	Compression string `json:"compression,omitempty"`
}

type File struct {
	Version int      `json:"version"`
	Remotes []Remote `json:"remotes"`
}

// DefaultPath は VARVE_CONFIG があればそれを、無ければ
// ユーザー設定ディレクトリ配下の既定パスを返す。
func DefaultPath() (string, error) {
	if p := os.Getenv("VARVE_CONFIG"); p != "" {
		return p, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(base, "varve", "config.json"), nil
}

// Load は設定を読む。ファイルが無い場合は空の設定を返す。
func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &File{Version: FormatVersion}, nil
	}
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Version > FormatVersion {
		return nil, fmt.Errorf("%s was written by a newer version (config version %d)", path, f.Version)
	}
	return &f, nil
}

// Save は一時ファイル経由でアトミックに書く。
func Save(path string, f *File) error {
	f.Version = FormatVersion
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (f *File) Get(name string) *Remote {
	for i := range f.Remotes {
		if f.Remotes[i].Name == name {
			return &f.Remotes[i]
		}
	}
	return nil
}

func (f *File) Add(r Remote) error {
	if f.Get(r.Name) != nil {
		return fmt.Errorf("remote %q already exists (remove it first to replace)", r.Name)
	}
	f.Remotes = append(f.Remotes, r)
	return nil
}

func (f *File) Remove(name string) error {
	for i := range f.Remotes {
		if f.Remotes[i].Name == name {
			f.Remotes = append(f.Remotes[:i], f.Remotes[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("remote %q not found", name)
}

// Sole は remote がちょうど 1 つのときだけそれを返す。
// 複数あるときの暗黙選択は事故のもとなので行わない。
func (f *File) Sole() *Remote {
	if len(f.Remotes) == 1 {
		return &f.Remotes[0]
	}
	return nil
}
