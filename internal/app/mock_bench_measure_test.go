package app

// A1 (測定記録リポジトリ win-vhdx-worktree の docs/bench-mock-repo.md) の測定ハーネス。通常のテスト実行ではスキップされる。
// MEASURE_MOCK=1 go test ./internal/app -run TestMeasureMockRepo -v -timeout 30m で実行する。
//
// 実在リポジトリが用意できないため、現実の形状を模したモックで測る:
//   - 小さいソースファイル多数（2〜16 KiB、git 管理相当）
//   - 中間サイズのドキュメント・データ（50〜200 KiB）
//   - 少数の大きいバイナリ（1〜8 MiB、ビルド成果物・パッケージ相当。
//     半分は圧縮が効く内容、半分は圧縮済み相当の乱数）

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type mockRepo struct {
	root    string
	rng     *rand.Rand
	sources []string // 小ファイルの相対パス
	bins    []string // 大バイナリの相対パス
}

// compressibleBytes はテキスト風の圧縮が効くデータを作る。
func (m *mockRepo) compressibleBytes(n int) []byte {
	words := []string{"func ", "return ", "if err != nil {\n", "package main\n", "// TODO: ", "const ", "var ", "import (\n"}
	var b strings.Builder
	b.Grow(n)
	for b.Len() < n {
		b.WriteString(words[m.rng.Intn(len(words))])
		fmt.Fprintf(&b, "value%d\n", m.rng.Intn(1000))
	}
	return []byte(b.String()[:n])
}

func (m *mockRepo) randomBytes(n int) []byte {
	buf := make([]byte, n)
	m.rng.Read(buf)
	return buf
}

func (m *mockRepo) write(t *testing.T, rel string, content []byte) {
	t.Helper()
	p := filepath.Join(m.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildMockRepo(t *testing.T, root string) *mockRepo {
	t.Helper()
	m := &mockRepo{root: root, rng: rand.New(rand.NewSource(42))}
	// 4,000 の小ファイル（2〜16 KiB、圧縮可能）
	for i := range 4000 {
		rel := filepath.Join(fmt.Sprintf("src/pkg%02d", i%40), fmt.Sprintf("file%04d.go", i))
		m.write(t, rel, m.compressibleBytes(2048+m.rng.Intn(14336)))
		m.sources = append(m.sources, rel)
	}
	// 500 の中間ファイル（50〜200 KiB、圧縮可能）
	for i := range 500 {
		rel := filepath.Join(fmt.Sprintf("docs/d%02d", i%10), fmt.Sprintf("doc%03d.md", i))
		m.write(t, rel, m.compressibleBytes(51200+m.rng.Intn(153600)))
	}
	// 30 の大バイナリ（1〜8 MiB）。半分は圧縮可能、半分は圧縮済み相当。
	for i := range 30 {
		rel := filepath.Join("build", fmt.Sprintf("artifact%02d.bin", i))
		size := (1 << 20) + m.rng.Intn(7<<20)
		if i%2 == 0 {
			m.write(t, rel, m.compressibleBytes(size))
		} else {
			m.write(t, rel, m.randomBytes(size))
		}
		m.bins = append(m.bins, rel)
	}
	return m
}

func treeSize(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(root, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			total += fi.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

// churn は 1 日分の開発を模す: ソース 3% を微修正（サイズ ±10%）、
// 50 ファイル追加、20 ファイル削除、バイナリ 5 個を作り直す。
func (m *mockRepo) churn(t *testing.T) (changedBytes int64) {
	t.Helper()
	for range len(m.sources) * 3 / 100 {
		rel := m.sources[m.rng.Intn(len(m.sources))]
		size := 2048 + m.rng.Intn(14336)
		m.write(t, rel, m.compressibleBytes(size))
		changedBytes += int64(size)
	}
	for i := range 50 {
		rel := filepath.Join("src/new", fmt.Sprintf("added%03d-%d.go", i, m.rng.Int()))
		size := 2048 + m.rng.Intn(14336)
		m.write(t, rel, m.compressibleBytes(size))
		m.sources = append(m.sources, rel)
		changedBytes += int64(size)
	}
	for range 20 {
		idx := m.rng.Intn(len(m.sources))
		os.Remove(filepath.Join(m.root, m.sources[idx]))
		m.sources = append(m.sources[:idx], m.sources[idx+1:]...)
	}
	for range 5 {
		rel := m.bins[m.rng.Intn(len(m.bins))]
		size := (1 << 20) + m.rng.Intn(7<<20)
		m.write(t, rel, m.compressibleBytes(size))
		changedBytes += int64(size)
	}
	return changedBytes
}

func chunkStats(t *testing.T, remote *memoryStore) (count int, bytes int64) {
	t.Helper()
	objs, err := remote.List(context.Background(), "chunks/")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		bytes += o.Size
	}
	return len(objs), bytes
}

func storeTotal(t *testing.T, remote *memoryStore) int64 {
	t.Helper()
	objs, err := remote.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, o := range objs {
		total += o.Size
	}
	return total
}

// measurePush はアップロード量を 2 通りで返す。addedBytes は chunk のみ、
// addedTotal は manifest（ファイル数 × 約 300 バイトの JSON）と ref を含む
// 総アップロード量。git の bare サイズと比較するときは addedTotal を使う。
func measurePush(t *testing.T, remote *memoryStore, name, source, codec string) (elapsed time.Duration, addedChunks int, addedBytes, addedTotal int64) {
	t.Helper()
	beforeCount, beforeBytes := chunkStats(t, remote)
	beforeTotal := storeTotal(t, remote)
	start := time.Now()
	if err := Push(context.Background(), remote, PushOptions{
		Name: name, Source: source, Kind: "tree", Codec: codec, Concurrency: 8,
	}); err != nil {
		t.Fatal(err)
	}
	elapsed = time.Since(start)
	afterCount, afterBytes := chunkStats(t, remote)
	return elapsed, afterCount - beforeCount, afterBytes - beforeBytes, storeTotal(t, remote) - beforeTotal
}

func TestMeasureMockRepo(t *testing.T) {
	if os.Getenv("MEASURE_MOCK") == "" {
		t.Skip("set MEASURE_MOCK=1 to run the measurement")
	}
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "mock")
	m := buildMockRepo(t, root)
	logical := treeSize(t, root)
	t.Logf("mock repo: %d files, logical size %.1f MB", 4000+500+30, float64(logical)/1e6)

	// ページキャッシュを温めてから測る。最初の 1 回だけ cold になり、
	// 先に実行したコーデックが不利に見える（README「測定するときの落とし穴」）。
	if err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			_, err = os.ReadFile(p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// --- 初回 push: gzip と zstd を別ストアで比較（warm、2 回ずつ） ---------
	for _, codec := range []string{CodecGzip, CodecZstd, CodecGzip, CodecZstd} {
		remote := newMemoryStore()
		elapsed, chunks, bytes, total := measurePush(t, remote, "mock", root, codec)
		t.Logf("initial push [%s]: %.1f MB chunks + %.1f MB manifest/ref = %.1f MB total (%d chunks, ratio %.1f%%), %.1fs",
			codec, float64(bytes)/1e6, float64(total-bytes)/1e6, float64(total)/1e6, chunks, 100*float64(total)/float64(logical), elapsed.Seconds())
	}

	// --- 世代 churn: 転送量が「変更ファイルのバイト数」に比例するか --------
	remote := newMemoryStore()
	if _, _, initialBytes, _ := measurePush(t, remote, "mock", root, CodecGzip); initialBytes == 0 {
		t.Fatal("initial push uploaded nothing")
	}
	for gen := 2; gen <= 4; gen++ {
		changed := m.churn(t)
		elapsed, chunks, bytes, total := measurePush(t, remote, "mock", root, CodecGzip)
		t.Logf("gen %d churn: changed %.1f MB -> uploaded %.2f MB chunks + %.2f MB manifest = %.2f MB total (%d chunks), %.1fs",
			gen, float64(changed)/1e6, float64(bytes)/1e6, float64(total-bytes)/1e6, float64(total)/1e6, chunks, elapsed.Seconds())
	}

	// --- 差分チャンク最適性 1: 小ファイル 1 個の変更 -------------------------
	target := m.sources[100]
	m.write(t, target, m.compressibleBytes(4096))
	_, chunks, bytes, _ := measurePush(t, remote, "mock", root, CodecGzip)
	t.Logf("single small-file change: uploaded %d chunks / %.1f KB (optimal = 1 chunk)", chunks, float64(bytes)/1e3)
	if chunks != 1 {
		t.Errorf("expected exactly 1 invalidated chunk for a single small-file change, got %d", chunks)
	}

	// --- 差分チャンク最適性 2: 大ファイル先頭への挿入（ファイル内境界ずれ） --
	bin := m.bins[1] // 乱数バイナリ
	binPath := filepath.Join(root, bin)
	original, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	shifted := append(m.randomBytes(16), original...) // 先頭 16 バイト挿入
	if err := os.WriteFile(binPath, shifted, 0o644); err != nil {
		t.Fatal(err)
	}
	_, chunks, bytes, _ = measurePush(t, remote, "mock", root, CodecGzip)
	expected := (len(shifted) + (1 << 20) - 1) / (1 << 20)
	t.Logf("16-byte insert at head of %.1f MB binary: uploaded %d chunks / %.1f MB (in-file boundary shift; whole file = %d chunks)",
		float64(len(original))/1e6, chunks, float64(bytes)/1e6, expected)

	// --- pull の復元検証（バイト一致） --------------------------------------
	dest := filepath.Join(t.TempDir(), "restore")
	start := time.Now()
	if err := Pull(ctx, remote, PullOptions{
		Reference: "mock", Destination: dest, CacheDir: filepath.Join(t.TempDir(), "cache"), Concurrency: 12,
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("pull (cold cache, in-memory store): %.1fs", time.Since(start).Seconds())
	restored, err := os.ReadFile(filepath.Join(dest, bin))
	if err != nil || len(restored) != len(shifted) {
		t.Fatalf("restore mismatch: %v", err)
	}
}
