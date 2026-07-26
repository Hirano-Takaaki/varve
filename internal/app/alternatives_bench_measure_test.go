package app

// A2 (測定記録リポジトリ win-vhdx-worktree の docs/bench-vs-alternatives.md) の測定ハーネス。通常のテスト実行ではスキップされる。
// MEASURE_ALT=1 go test ./internal/app -run TestMeasureVsAlternatives -v -timeout 30m で実行する。
//
// A1 と同じモック（buildMockRepo / churn）を使い、初回配布と差分更新の
// 転送量を varve / git / 素のファイルコピーで比較する。git は現実の運用に
// 合わせて build/ を .gitignore し、ソースとドキュメントだけを管理する。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=bench", "GIT_AUTHOR_EMAIL=bench@example.com",
		"GIT_COMMITTER_NAME=bench", "GIT_COMMITTER_EMAIL=bench@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func dirSize(t *testing.T, root string) int64 {
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

// copyTracked は build/ と .git/ を除いたツリーの複製を作る。
// git と varve を同一ペイロードで比較するため。
func copyTracked(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			if rel == "build" || rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMeasureVsAlternatives(t *testing.T) {
	if os.Getenv("MEASURE_ALT") == "" {
		t.Skip("set MEASURE_ALT=1 to run the measurement")
	}
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "mock")
	m := buildMockRepo(t, root)
	logical := treeSize(t, root)

	// git は build/ を管理しない（現実の運用）。
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("/build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	buildSize := dirSize(t, filepath.Join(root, "build"))
	tracked := logical - buildSize
	t.Logf("mock: logical %.1f MB (git-tracked %.1f MB + build artifacts %.1f MB)",
		float64(logical)/1e6, float64(tracked)/1e6, float64(buildSize)/1e6)

	runGit(t, root, "init", "-q")
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-q", "-m", "gen1")

	// --- 初回配布 -----------------------------------------------------------
	// git: --no-local で pack 転送相当を強制した bare clone のサイズ。
	bare := filepath.Join(t.TempDir(), "bare.git")
	runGit(t, filepath.Dir(bare), "clone", "-q", "--bare", "--no-local", root, bare)
	gitInitial := dirSize(t, bare)

	remote := newMemoryStore()
	_, _, _, winInitial := measurePush(t, remote, "mock", root, CodecGzip)

	// 同一ペイロード比較: git 管理分（build/ と .git を除く）だけを varve で運ぶ。
	trackedRemote := newMemoryStore()
	tracked1 := filepath.Join(t.TempDir(), "tracked1")
	copyTracked(t, root, tracked1)
	_, _, _, winTrackedInitial := measurePush(t, trackedRemote, "tracked", tracked1, CodecGzip)

	t.Logf("initial: raw copy %.1f MB | git clone %.1f MB | varve same-payload %.1f MB | varve everything %.1f MB (all incl. manifest)",
		float64(logical)/1e6, float64(gitInitial)/1e6, float64(winTrackedInitial)/1e6, float64(winInitial)/1e6)

	// --- 差分更新 -----------------------------------------------------------
	changed := m.churn(t)
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-q", "-m", "gen2")

	before := dirSize(t, bare)
	runGit(t, bare, "fetch", "-q", "origin", "+HEAD:refs/heads/bench")
	gitDelta := dirSize(t, bare) - before

	_, _, _, winDelta := measurePush(t, remote, "mock", root, CodecGzip)

	tracked2 := filepath.Join(t.TempDir(), "tracked2")
	copyTracked(t, root, tracked2)
	_, _, _, winTrackedDelta := measurePush(t, trackedRemote, "tracked", tracked2, CodecGzip)

	t.Logf("update: changed %.1f MB | git fetch %.1f MB | varve same-payload %.2f MB | varve everything %.2f MB (all incl. manifest)",
		float64(changed)/1e6, float64(gitDelta)/1e6, float64(winTrackedDelta)/1e6, float64(winDelta)/1e6)

	// --- 受け取り側に前世代が無い場合の varve（cache も seed も無し） -----
	dest := filepath.Join(t.TempDir(), "fresh")
	if err := Pull(ctx, remote, PullOptions{
		Reference: "mock", Destination: dest,
		CacheDir: filepath.Join(t.TempDir(), "cache-fresh"), Concurrency: 12,
	}); err != nil {
		t.Fatal(err)
	}
	var downloadedBytes int64
	remote.mu.Lock()
	for key, n := range remote.gets {
		if len(key) > 7 && key[:7] == "chunks/" && n > 0 {
			downloadedBytes += int64(len(remote.data[key])) * int64(n)
		}
	}
	remote.mu.Unlock()
	t.Logf("fresh pull downloads %.1f MB (= current generation's referenced chunks, compressed)",
		float64(downloadedBytes)/1e6)
}
