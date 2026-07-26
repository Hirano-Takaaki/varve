package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hirano-Takaaki/varve/internal/model"
	"github.com/Hirano-Takaaki/varve/internal/store"
)

type memoryStore struct {
	mu       sync.Mutex
	data     map[string][]byte
	mtime    map[string]time.Time
	puts     int
	gets     map[string]int
	heads    int
	deletes  int
	failList error // List を疑似的に失敗させる（疎通確認のテスト用）
}

func newMemoryStore() *memoryStore {
	return &memoryStore{data: make(map[string][]byte), mtime: make(map[string]time.Time), gets: make(map[string]int)}
}
func (m *memoryStore) Key(parts ...string) string { return strings.Join(parts, "/") }
func (m *memoryStore) Put(_ context.Context, k string, v []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[k] = append([]byte(nil), v...)
	m.mtime[k] = time.Now().UTC()
	m.puts++
	return nil
}

func (m *memoryStore) Delete(_ context.Context, k string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, k)
	delete(m.mtime, k)
	m.deletes++
	return nil
}

// touch は grace 検証用にオブジェクトの更新時刻を差し替える。
func (m *memoryStore) touch(k string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mtime[k] = at
}
func (m *memoryStore) Get(_ context.Context, k string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets[k]++
	v, ok := m.data[k]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), v...), nil
}
func (m *memoryStore) Exists(_ context.Context, k string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.heads++
	_, ok := m.data[k]
	return ok, nil
}
func (m *memoryStore) List(_ context.Context, prefix string) ([]store.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failList != nil {
		return nil, m.failList
	}
	var out []store.Object
	for k, v := range m.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, store.Object{Key: k, Size: int64(len(v)), LastModified: m.mtime[k]})
		}
	}
	return out, nil
}

func TestPushPullTreeAndReuse(t *testing.T) {
	ctx := context.Background()
	remote := newMemoryStore()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(source, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "hello.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "sub", "zero.bin"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := PushOptions{Name: "project", Source: source, Kind: "tree", Concurrency: 2}
	if err := Push(ctx, remote, opts); err != nil {
		t.Fatal(err)
	}
	firstPuts := remote.puts
	if err := Push(ctx, remote, opts); err != nil {
		t.Fatal(err)
	}
	if got := remote.puts - firstPuts; got != 0 {
		t.Fatalf("unchanged second push wrote %d objects, want 0", got)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	if err := Pull(ctx, remote, PullOptions{
		Reference: "project", Destination: destination, CacheDir: filepath.Join(t.TempDir(), "cache"), Concurrency: 2,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello world" {
		t.Fatalf("restored content = %q", got)
	}
	fi, err := os.Stat(filepath.Join(destination, "sub", "zero.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 1<<20 {
		t.Fatalf("zero file size = %d", fi.Size())
	}
}

func TestPushOnlyChecksAndUploadsChangedFixedChunk(t *testing.T) {
	ctx := context.Background()
	remote := newMemoryStore()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(source, "disk.bin")
	content := make([]byte, 3*model.DefaultChunkSize)
	for i := range 3 {
		for j := int64(0); j < model.DefaultChunkSize; j++ {
			content[int64(i)*model.DefaultChunkSize+j] = byte(i + 1)
		}
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	options := PushOptions{
		Name: "fixed", Source: source, Kind: "tree", Concurrency: 2,
		ChunkSize: model.DefaultChunkSize,
	}
	if err := Push(ctx, remote, options); err != nil {
		t.Fatal(err)
	}
	var firstRef model.Ref
	if err := json.Unmarshal(remote.data["refs/fixed/latest.json"], &firstRef); err != nil {
		t.Fatal(err)
	}

	content[model.DefaultChunkSize+17] ^= 0xff
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	remote.mu.Lock()
	beforePuts := remote.puts
	remote.heads = 0
	remote.mu.Unlock()
	if err := Push(ctx, remote, options); err != nil {
		t.Fatal(err)
	}

	remote.mu.Lock()
	addedPuts := remote.puts - beforePuts
	headRequests := remote.heads
	remote.mu.Unlock()
	if addedPuts != 3 { // one changed chunk, one immutable manifest, one latest ref
		t.Fatalf("changed push wrote %d objects, want 3", addedPuts)
	}
	if headRequests != 4 { // gc lock + latest ref + the unknown chunk (.gz miss, then .zst for cross-codec reuse)
		t.Fatalf("changed push made %d HEAD requests, want 4", headRequests)
	}
	var secondRef model.Ref
	if err := json.Unmarshal(remote.data["refs/fixed/latest.json"], &secondRef); err != nil {
		t.Fatal(err)
	}
	secondJSON, err := gunzipBytes(remote.data["snapshots/fixed/"+secondRef.SnapshotID+".json.gz"])
	if err != nil {
		t.Fatal(err)
	}
	var second model.Manifest
	if err := json.Unmarshal(secondJSON, &second); err != nil {
		t.Fatal(err)
	}
	if second.ParentID != firstRef.SnapshotID {
		t.Fatalf("parent = %q, want %q", second.ParentID, firstRef.SnapshotID)
	}
	if second.ChunkSize != model.DefaultChunkSize {
		t.Fatalf("chunk size = %d, want %d", second.ChunkSize, model.DefaultChunkSize)
	}
	var history bytes.Buffer
	if err := History(ctx, remote, "fixed", &history); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(history.String(), secondRef.SnapshotID+"\t"+firstRef.SnapshotID) ||
		!strings.Contains(history.String(), firstRef.SnapshotID+"\tnone") {
		t.Fatalf("history does not contain the parent chain:\n%s", history.String())
	}
}

func TestPushSchedulesRepeatedContentOnce(t *testing.T) {
	remote := newMemoryStore()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte{0x7b}, int(model.DefaultChunkSize))
	if err := os.WriteFile(filepath.Join(source, "repeated.bin"), bytes.Repeat(chunk, 3), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Push(context.Background(), remote, PushOptions{
		Name: "dedup", Source: source, Kind: "tree", Concurrency: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if remote.puts != 3 { // one unique chunk, one manifest, one ref
		t.Fatalf("initial repeated-content push wrote %d objects, want 3", remote.puts)
	}
	if remote.heads != 4 { // gc lock, latest ref, and one unique chunk (.gz miss, then .zst for cross-codec reuse)
		t.Fatalf("initial repeated-content push made %d HEAD requests, want 4", remote.heads)
	}
}

func TestPullVHDXUsesExistingDestinationAsSeed(t *testing.T) {
	ctx := context.Background()
	remote := newMemoryStore()
	chunkSize := model.DefaultChunkSize
	a := bytes.Repeat([]byte{0x11}, int(chunkSize))
	b := bytes.Repeat([]byte{0x22}, int(chunkSize))
	c := bytes.Repeat([]byte{0x33}, int(chunkSize))
	d := bytes.Repeat([]byte{0x44}, int(chunkSize))
	changed := bytes.Repeat([]byte{0x55}, int(chunkSize))
	zero := make([]byte, int(chunkSize))
	old := bytes.Join([][]byte{a, b, d, c}, nil)
	want := bytes.Join([][]byte{c, changed, zero, a}, nil)

	chunks := []model.Chunk{
		{Hash: hash(c), Size: chunkSize},
		{Hash: hash(changed), Size: chunkSize},
		{Hash: hash(zero), Size: chunkSize, Zero: true},
		{Hash: hash(a), Size: chunkSize},
	}
	manifest := model.Manifest{
		Version: model.FormatVersion, Name: "image", ID: "snapshot-2", Kind: "vhdx",
		ChunkSize: chunkSize, Size: int64(len(want)),
		Files: []model.File{{Path: "dev.vhdx", Size: int64(len(want)), Chunks: chunks}},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	refJSON, err := json.Marshal(model.Ref{Name: "image", SnapshotID: manifest.ID})
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	zw, err := gzip.NewWriterLevel(&encoded, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(changed); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	remote.data["refs/image/latest.json"] = refJSON
	remote.data["snapshots/image/snapshot-2.json"] = manifestJSON
	remote.data["chunks/"+hash(changed)[:2]+"/"+hash(changed)+".gz"] = encoded.Bytes()

	destination := filepath.Join(t.TempDir(), "dev.vhdx")
	if err := os.WriteFile(destination, old, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Pull(ctx, remote, PullOptions{
		Reference: "image", Destination: destination, CacheDir: filepath.Join(t.TempDir(), "cache"),
		Concurrency: 2, Force: true,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("seeded VHDX restore did not match the target")
	}
	changedKey := "chunks/" + hash(changed)[:2] + "/" + hash(changed) + ".gz"
	if remote.gets[changedKey] != 1 {
		t.Fatalf("changed chunk downloads = %d, want 1", remote.gets[changedKey])
	}
	for _, unchanged := range [][]byte{a, c} {
		key := "chunks/" + hash(unchanged)[:2] + "/" + hash(unchanged) + ".gz"
		if remote.gets[key] != 0 {
			t.Fatalf("unchanged chunk %s was downloaded", hash(unchanged))
		}
	}
}

func TestSafeDestinationRejectsRootAndCWD(t *testing.T) {
	if _, err := safeDestination(filepath.VolumeName(t.TempDir()) + string(filepath.Separator)); err == nil {
		t.Fatal("volume root was accepted")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := safeDestination(cwd); err == nil {
		t.Fatal("cwd was accepted")
	}
}

func BenchmarkFingerprint1MiB(b *testing.B) {
	for _, benchmark := range []struct {
		name string
		data []byte
	}{
		{name: "nonzero", data: bytes.Repeat([]byte{0xa5}, int(model.DefaultChunkSize))},
		{name: "zero", data: make([]byte, int(model.DefaultChunkSize))},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.SetBytes(model.DefaultChunkSize)
			var fingerprints chunkFingerprinter
			for b.Loop() {
				_, _ = fingerprints.fingerprint(benchmark.data)
			}
		})
	}
}
