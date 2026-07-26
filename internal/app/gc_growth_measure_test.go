package app

// A3 (docs/design-gc.md) の測定ハーネス。通常のテスト実行ではスキップされる。
// MEASURE_GC=1 go test ./internal/app -run TestMeasureStoreGrowth -v で実行する。

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hirano-Takaaki/varve/internal/model"
)

func TestMeasureStoreGrowth(t *testing.T) {
	if os.Getenv("MEASURE_GC") == "" {
		t.Skip("set MEASURE_GC=1 to run the measurement")
	}
	const (
		fileCount   = 500
		fileSize    = 64 << 10 // 64 KiB
		generations = 10
		churnRatio  = 0.05 // 世代ごとに 5% のファイルを書き換える
	)
	ctx := context.Background()
	remote := newMemoryStore()
	source := filepath.Join(t.TempDir(), "src")
	rng := rand.New(rand.NewSource(1))
	writeFile := func(i int) {
		buf := make([]byte, fileSize)
		rng.Read(buf)
		p := filepath.Join(source, fmt.Sprintf("d%02d", i%20), fmt.Sprintf("f%04d.bin", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := range fileCount {
		writeFile(i)
	}

	storeSize := func() (total int64, chunkBytes int64, chunks int) {
		objs, _ := remote.List(ctx, "")
		for _, o := range objs {
			total += o.Size
			if strings.HasPrefix(o.Key, "chunks/") {
				chunkBytes += o.Size
				chunks++
			}
		}
		return
	}

	t.Logf("gen | store total | chunk bytes | chunks | delta")
	var prev int64
	for gen := 1; gen <= generations; gen++ {
		if gen > 1 {
			for range int(fileCount * churnRatio) {
				writeFile(rng.Intn(fileCount))
			}
		}
		if err := Push(ctx, remote, PushOptions{Name: "growth", Source: source, Kind: "tree", Concurrency: 4}); err != nil {
			t.Fatal(err)
		}
		total, cb, n := storeSize()
		t.Logf("%3d | %11d | %11d | %6d | %d", gen, total, cb, n, total-prev)
		prev = total
	}

	// gc の回収見込み: 直近 keep 世代の manifest から参照集合を作り、
	// chunks/ の未参照分を数える（mark-and-sweep の実現可能性確認）。
	for _, keep := range []int{1, 3, 5} {
		referenced := make(map[string]struct{})
		objs, _ := remote.List(ctx, "snapshots/growth/")
		// created_at 順に並べるため manifest を全部読む
		var manifests []model.Manifest
		for _, o := range objs {
			raw, err := remote.Get(ctx, o.Key)
			if err != nil {
				t.Fatal(err)
			}
			var m model.Manifest
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			manifests = append(manifests, m)
		}
		// parent chain を latest からたどる
		byID := make(map[string]model.Manifest)
		children := make(map[string]bool)
		for _, m := range manifests {
			byID[m.ID] = m
			if m.ParentID != "" {
				children[m.ParentID] = true
			}
		}
		var head model.Manifest
		for _, m := range manifests {
			if !children[m.ID] {
				head = m
			}
		}
		cur, kept := head, 0
		for kept < keep {
			for _, f := range cur.Files {
				for _, ch := range f.Chunks {
					if !ch.Zero {
						referenced[ch.Hash] = struct{}{}
					}
				}
			}
			kept++
			if cur.ParentID == "" {
				break
			}
			cur = byID[cur.ParentID]
		}
		var reclaimable, totalChunks int64
		var reclaimCount int
		chunkObjs, _ := remote.List(ctx, "chunks/")
		for _, o := range chunkObjs {
			totalChunks += o.Size
			hash := strings.TrimSuffix(filepath.Base(o.Key), ".gz")
			if _, ok := referenced[hash]; !ok {
				reclaimable += o.Size
				reclaimCount++
			}
		}
		t.Logf("keep=%d: reclaimable %d / %d bytes (%.1f%%), %d chunks", keep, reclaimable, totalChunks,
			100*float64(reclaimable)/float64(totalChunks), reclaimCount)
	}
}
