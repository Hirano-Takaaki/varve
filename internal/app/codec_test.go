package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSource(t *testing.T, files map[string][]byte) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "src")
	for name, content := range files {
		p := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return source
}

func pullTo(t *testing.T, remote *memoryStore, ref string) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "restore")
	if err := Pull(context.Background(), remote, PullOptions{
		Reference: ref, Destination: dest,
		CacheDir: filepath.Join(t.TempDir(), "cache"), Concurrency: 2,
	}); err != nil {
		t.Fatal(err)
	}
	return dest
}

func TestNormalizeCodec(t *testing.T) {
	for input, want := range map[string]string{"": CodecGzip, "gzip": CodecGzip, "zstd": CodecZstd} {
		got, err := NormalizeCodec(input)
		if err != nil || got != want {
			t.Fatalf("NormalizeCodec(%q) = %q, %v", input, got, err)
		}
	}
	if _, err := NormalizeCodec("lz4"); err == nil {
		t.Fatal("expected unsupported codec to fail")
	}
}

func TestPushZstdRoundTrip(t *testing.T) {
	ctx := context.Background()
	remote := newMemoryStore()
	content := bytes.Repeat([]byte("varve zstd "), 512)
	source := writeSource(t, map[string][]byte{"data.bin": content})

	if err := Push(ctx, remote, PushOptions{
		Name: "proj", Source: source, Kind: "tree", Codec: CodecZstd, Concurrency: 2,
	}); err != nil {
		t.Fatal(err)
	}
	objs, err := remote.List(ctx, "chunks/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) == 0 {
		t.Fatal("no chunks written")
	}
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".zst") {
			t.Fatalf("expected .zst chunk keys, got %s", o.Key)
		}
	}

	dest := pullTo(t, remote, "proj")
	restored, err := os.ReadFile(filepath.Join(dest, "data.bin"))
	if err != nil || !bytes.Equal(restored, content) {
		t.Fatalf("zstd round trip failed: %v", err)
	}
}

func TestMixedCodecHistoryRestores(t *testing.T) {
	ctx := context.Background()
	remote := newMemoryStore()
	stable := bytes.Repeat([]byte("stable "), 1024)
	source := writeSource(t, map[string][]byte{"stable.bin": stable, "churn.txt": []byte("v1")})

	// 世代 1 は gzip、世代 2 は zstd。stable.bin の chunk は gzip のまま継承される。
	if err := Push(ctx, remote, PushOptions{Name: "proj", Source: source, Kind: "tree", Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "churn.txt"), []byte("v2 with new bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Push(ctx, remote, PushOptions{Name: "proj", Source: source, Kind: "tree", Codec: CodecZstd, Concurrency: 2}); err != nil {
		t.Fatal(err)
	}

	hasGz, hasZst := false, false
	objs, err := remote.List(ctx, "chunks/")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		hasGz = hasGz || strings.HasSuffix(o.Key, ".gz")
		hasZst = hasZst || strings.HasSuffix(o.Key, ".zst")
	}
	if !hasGz || !hasZst {
		t.Fatalf("expected mixed codecs in the store (gz=%v zst=%v)", hasGz, hasZst)
	}

	dest := pullTo(t, remote, "proj")
	restoredStable, err := os.ReadFile(filepath.Join(dest, "stable.bin"))
	if err != nil || !bytes.Equal(restoredStable, stable) {
		t.Fatalf("inherited gzip chunk failed to restore: %v", err)
	}
	restoredChurn, err := os.ReadFile(filepath.Join(dest, "churn.txt"))
	if err != nil || string(restoredChurn) != "v2 with new bytes" {
		t.Fatalf("zstd chunk failed to restore: %q, %v", restoredChurn, err)
	}
}

func TestCrossCodecDedupReusesExistingChunk(t *testing.T) {
	ctx := context.Background()
	remote := newMemoryStore()
	shared := bytes.Repeat([]byte{0x5a}, 8192)
	source := writeSource(t, map[string][]byte{"shared.bin": shared})

	if err := Push(ctx, remote, PushOptions{Name: "alpha", Source: source, Kind: "tree", Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	chunksBefore := countPrefix(t, remote, "chunks/")

	// 同じ内容を zstd 指定で別名 push しても、既存の .gz chunk を再利用し
	// 重複オブジェクトを作らない。
	if err := Push(ctx, remote, PushOptions{Name: "beta", Source: source, Kind: "tree", Codec: CodecZstd, Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	if got := countPrefix(t, remote, "chunks/"); got != chunksBefore {
		t.Fatalf("cross-codec dedup failed: chunks %d -> %d", chunksBefore, got)
	}

	dest := pullTo(t, remote, "beta")
	restored, err := os.ReadFile(filepath.Join(dest, "shared.bin"))
	if err != nil || !bytes.Equal(restored, shared) {
		t.Fatalf("beta restore failed: %v", err)
	}
}
