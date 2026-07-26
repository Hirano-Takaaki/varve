package model

import "time"

const (
	FormatVersion    = 1
	DefaultChunkSize = int64(1 << 20)
)

type Manifest struct {
	Version   int       `json:"version"`
	Name      string    `json:"name"`
	ID        string    `json:"id"`
	ParentID  string    `json:"parent_id,omitempty"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	ChunkSize int64     `json:"chunk_size"`
	Size      int64     `json:"size"`
	Files     []File    `json:"files"`
}

type File struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mode    uint32    `json:"mode,omitempty"`
	ModTime time.Time `json:"mod_time,omitempty"`
	Chunks  []Chunk   `json:"chunks,omitempty"`
}

type Chunk struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	Zero bool   `json:"zero,omitempty"`
	// Codec はオブジェクトの圧縮形式。空は gzip（旧形式との互換）。
	Codec string `json:"codec,omitempty"`
}

type Ref struct {
	Name       string    `json:"name"`
	SnapshotID string    `json:"snapshot_id"`
	CreatedAt  time.Time `json:"created_at"`
	Kind       string    `json:"kind"`
	ChunkSize  int64     `json:"chunk_size,omitempty"`
	Size       int64     `json:"size"`
}
