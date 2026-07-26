package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pushGenerations は 1 ファイルを書き換えながら n 世代 push する。
func pushGenerations(t *testing.T, remote *memoryStore, name string, n int) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "stable.txt"), []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	for gen := 1; gen <= n; gen++ {
		if err := os.WriteFile(filepath.Join(source, "churn.txt"), []byte(fmt.Sprintf("generation %d", gen)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Push(context.Background(), remote, PushOptions{Name: name, Source: source, Kind: "tree", Concurrency: 2}); err != nil {
			t.Fatal(err)
		}
	}
	return source
}

func countPrefix(t *testing.T, remote *memoryStore, prefix string) int {
	t.Helper()
	objs, err := remote.List(context.Background(), prefix)
	if err != nil {
		t.Fatal(err)
	}
	return len(objs)
}

func TestGCDryRunDeletesNothing(t *testing.T) {
	remote := newMemoryStore()
	pushGenerations(t, remote, "proj", 4)
	before := countPrefix(t, remote, "")
	var report bytes.Buffer
	if err := GC(context.Background(), remote, GCOptions{Keep: 1, Grace: 0, Progress: &report}); err != nil {
		t.Fatal(err)
	}
	if got := countPrefix(t, remote, ""); got != before {
		t.Fatalf("dry-run changed the store: %d -> %d objects", before, got)
	}
	if remote.deletes != 0 {
		t.Fatalf("dry-run issued %d deletes", remote.deletes)
	}
	if !strings.Contains(report.String(), "dry-run") {
		t.Fatalf("report: %q", report.String())
	}
}

func TestGCKeepPrunesOldGenerations(t *testing.T) {
	ctx := context.Background()
	remote := newMemoryStore()
	pushGenerations(t, remote, "proj", 5)
	if got := countPrefix(t, remote, "snapshots/proj/"); got != 5 {
		t.Fatalf("expected 5 snapshots before gc, got %d", got)
	}

	var report bytes.Buffer
	if err := GC(ctx, remote, GCOptions{Keep: 2, Delete: true, Grace: 0, Progress: &report}); err != nil {
		t.Fatal(err)
	}
	if got := countPrefix(t, remote, "snapshots/proj/"); got != 2 {
		t.Fatalf("expected 2 snapshots after gc, got %d", got)
	}
	// churn.txt の世代 1〜3 の chunk は未参照になり消える。stable.txt の
	// chunk は全世代で共有されるため残る。
	if !strings.Contains(report.String(), "proj: keep 2 generations, prune 3") {
		t.Fatalf("report: %q", report.String())
	}

	// 残った latest は完全に復元できる。
	dest := filepath.Join(t.TempDir(), "restore")
	if err := Pull(ctx, remote, PullOptions{
		Reference: "proj", Destination: dest, CacheDir: filepath.Join(t.TempDir(), "cache"), Concurrency: 2,
	}); err != nil {
		t.Fatalf("pull after gc: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dest, "churn.txt"))
	if err != nil || string(raw) != "generation 5" {
		t.Fatalf("restored churn.txt = %q, %v", raw, err)
	}

	// history は prune 済みの親で途切れて正常終了する。
	var hist bytes.Buffer
	if err := History(ctx, remote, "proj", &hist); err != nil {
		t.Fatalf("history after gc: %v", err)
	}
	if !strings.Contains(hist.String(), "(pruned)") {
		t.Fatalf("history output: %q", hist.String())
	}

	// ロックは解放されている。
	if locked, _ := remote.Exists(ctx, LockKey); locked {
		t.Fatal("gc.lock left behind")
	}
}

func TestGCProtectsChunksSharedAcrossNames(t *testing.T) {
	ctx := context.Background()
	remote := newMemoryStore()
	// 同じ内容を別名で push し、chunk を共有させる。
	source := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "shared.bin"), bytes.Repeat([]byte("x"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		if err := Push(ctx, remote, PushOptions{Name: name, Source: source, Kind: "tree", Concurrency: 2}); err != nil {
			t.Fatal(err)
		}
	}
	// alpha だけ世代を進めて古い世代を作る。
	if err := os.WriteFile(filepath.Join(source, "shared.bin"), bytes.Repeat([]byte("y"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Push(ctx, remote, PushOptions{Name: "alpha", Source: source, Kind: "tree", Concurrency: 2}); err != nil {
		t.Fatal(err)
	}

	if err := GC(ctx, remote, GCOptions{Keep: 1, Delete: true, Grace: 0, Progress: nil}); err != nil {
		t.Fatal(err)
	}
	// beta は旧内容のままなので復元できなければならない。
	dest := filepath.Join(t.TempDir(), "beta")
	if err := Pull(ctx, remote, PullOptions{
		Reference: "beta", Destination: dest, CacheDir: filepath.Join(t.TempDir(), "cache"), Concurrency: 2,
	}); err != nil {
		t.Fatalf("pull beta after gc: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dest, "shared.bin"))
	if err != nil || !bytes.Equal(raw, bytes.Repeat([]byte("x"), 4096)) {
		t.Fatalf("beta content corrupted after gc: %v", err)
	}
}

func TestGCGraceProtectsFreshObjects(t *testing.T) {
	ctx := context.Background()
	remote := newMemoryStore()
	pushGenerations(t, remote, "proj", 3)
	// 全 chunk はたった今書かれたので、1 時間の猶予内では消えない。
	if err := GC(ctx, remote, GCOptions{Keep: 1, Delete: true, Grace: time.Hour, Progress: nil}); err != nil {
		t.Fatal(err)
	}
	if got := countPrefix(t, remote, "snapshots/proj/"); got != 1 {
		t.Fatalf("keep 超過世代の manifest は猶予に関係なく消える: got %d", got)
	}
	objs, err := remote.List(ctx, "chunks/")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		remote.touch(o.Key, time.Now().Add(-2*time.Hour))
	}
	// 猶予を過ぎれば未参照 chunk は消える。
	before := countPrefix(t, remote, "chunks/")
	if err := GC(ctx, remote, GCOptions{Keep: 1, Delete: true, Grace: time.Hour, Progress: nil}); err != nil {
		t.Fatal(err)
	}
	after := countPrefix(t, remote, "chunks/")
	if after >= before {
		t.Fatalf("expected unreferenced chunks to be swept after grace: %d -> %d", before, after)
	}
}

func TestGCOrphanManifestRespectsGrace(t *testing.T) {
	ctx := context.Background()
	remote := newMemoryStore()
	pushGenerations(t, remote, "proj", 1)
	// ref を消して孤児化を再現する。
	if err := remote.Delete(ctx, "refs/proj/latest.json"); err != nil {
		t.Fatal(err)
	}
	remote.deletes = 0

	// 猶予内: 孤児 manifest と参照 chunk は残る。
	if err := GC(ctx, remote, GCOptions{Keep: 1, Delete: true, Grace: time.Hour, Progress: nil}); err != nil {
		t.Fatal(err)
	}
	if got := countPrefix(t, remote, "snapshots/proj/"); got != 1 {
		t.Fatalf("orphan within grace must survive, got %d snapshots", got)
	}

	// 猶予切れ: 孤児は chunk ごと消える。
	for _, prefix := range []string{"snapshots/", "chunks/"} {
		objs, err := remote.List(ctx, prefix)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range objs {
			remote.touch(o.Key, time.Now().Add(-2*time.Hour))
		}
	}
	if err := GC(ctx, remote, GCOptions{Keep: 1, Delete: true, Grace: time.Hour, Progress: nil}); err != nil {
		t.Fatal(err)
	}
	if got := countPrefix(t, remote, "snapshots/"); got != 0 {
		t.Fatalf("expired orphan must be deleted, got %d snapshots", got)
	}
	if got := countPrefix(t, remote, "chunks/"); got != 0 {
		t.Fatalf("expired orphan chunks must be deleted, got %d", got)
	}
}

func TestPushRefusesDuringGCLock(t *testing.T) {
	ctx := context.Background()
	remote := newMemoryStore()
	if err := remote.Put(ctx, LockKey, []byte(`{}`), "application/json"); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Push(ctx, remote, PushOptions{Name: "proj", Source: source, Kind: "tree", Concurrency: 1})
	if err == nil || !strings.Contains(err.Error(), "gc is running") {
		t.Fatalf("expected lock refusal, got %v", err)
	}
	if err := Push(ctx, remote, PushOptions{Name: "proj", Source: source, Kind: "tree", Concurrency: 1, IgnoreLock: true}); err != nil {
		t.Fatalf("push with IgnoreLock: %v", err)
	}
}

func TestGCRefusesWhenLockPresent(t *testing.T) {
	ctx := context.Background()
	remote := newMemoryStore()
	pushGenerations(t, remote, "proj", 2)
	if err := remote.Put(ctx, LockKey, []byte(`{}`), "application/json"); err != nil {
		t.Fatal(err)
	}
	err := GC(ctx, remote, GCOptions{Keep: 1, Delete: true, Grace: 0, Progress: nil})
	if err == nil || !strings.Contains(err.Error(), "another gc") {
		t.Fatalf("expected lock refusal, got %v", err)
	}
}
