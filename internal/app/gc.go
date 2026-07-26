package app

// gc は docs/design-gc.md の要件に基づく mark-and-sweep 実装。
// 既定は dry-run で、削除は GCOptions.Delete が真のときだけ行う。
// 並行 push との競合は「アドバイザリロック + 猶予期間 + 削除直前の再マーク」で緩和する。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/Hirano-Takaaki/varve/internal/model"
)

// GCStore は gc が要求する削除付きの store。push / pull は BlobStore の
// ままにし、DeleteObject 権限を要求するのは gc だけに留める。
type GCStore interface {
	BlobStore
	Delete(context.Context, string) error
}

type GCOptions struct {
	Keep     int
	Delete   bool
	Grace    time.Duration
	Progress io.Writer
}

// LockKey は gc 実行中に置くアドバイザリロック。refs/ の下に置くと
// list がこれを ref として解釈してしまうため、prefix 直下に置く。
const LockKey = "gc.lock"

type gcPlan struct {
	// key → オブジェクトサイズ。manifest と chunk を分けて持つ。
	manifests     map[string]int64
	chunks        map[string]int64
	keptPerName   map[string]int
	prunedPerName map[string]int
}

func GC(ctx context.Context, remote GCStore, o GCOptions) error {
	if o.Keep < 1 {
		return errors.New("keep must be at least 1")
	}
	if o.Grace < 0 {
		return errors.New("grace must not be negative")
	}
	out := o.Progress
	if out == nil {
		out = io.Discard
	}
	cutoff := time.Now().UTC().Add(-o.Grace)

	plan, err := markSweepPlan(ctx, remote, o.Keep, cutoff)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(plan.keptPerName))
	for name := range plan.keptPerName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(out, "%s: keep %d generations, prune %d\n",
			name, plan.keptPerName[name], plan.prunedPerName[name])
	}
	var chunkBytes int64
	for _, size := range plan.chunks {
		chunkBytes += size
	}
	// manifest は 1 snapshot が新旧両形式で存在しうるので、数はオブジェクト単位。
	fmt.Fprintf(out, "reclaimable: %d snapshot objects, %d chunks (%s)\n",
		len(plan.manifests), len(plan.chunks), formatBytes(chunkBytes))
	if !o.Delete {
		fmt.Fprintln(out, "dry-run: nothing deleted (pass --delete to reclaim)")
		return nil
	}

	lockKey := remote.Key(LockKey)
	locked, err := remote.Exists(ctx, lockKey)
	if err != nil {
		return fmt.Errorf("check gc lock: %w", err)
	}
	if locked {
		return fmt.Errorf("another gc appears to be running (%s exists; delete it manually if stale)", lockKey)
	}
	lockBody, _ := json.Marshal(map[string]string{"started_at": time.Now().UTC().Format(time.RFC3339)})
	if err := remote.Put(ctx, lockKey, lockBody, "application/json"); err != nil {
		return fmt.Errorf("acquire gc lock: %w", err)
	}
	defer func() {
		// ロック解除の失敗は gc 自体の失敗にしない（次回実行時に stale 判断を促す）。
		_ = remote.Delete(context.WithoutCancel(ctx), lockKey)
	}()

	// 再マーク: mark 開始後に発行された snapshot を守るため、ロック取得後に
	// もう一度計画を立て、両方の計画に載ったものだけを削除する。
	second, err := markSweepPlan(ctx, remote, o.Keep, cutoff)
	if err != nil {
		return err
	}
	deleted := 0
	var deletedBytes int64
	// manifest を先に消す。逆順だと途中失敗で「manifest はあるが chunk が
	// 無い」世代を作ってしまう。
	for key := range plan.manifests {
		if _, stillCandidate := second.manifests[key]; !stillCandidate {
			continue
		}
		if err := remote.Delete(ctx, key); err != nil {
			return fmt.Errorf("delete %s: %w", key, err)
		}
		deleted++
	}
	for key, size := range plan.chunks {
		if _, stillCandidate := second.chunks[key]; !stillCandidate {
			continue
		}
		if err := remote.Delete(ctx, key); err != nil {
			return fmt.Errorf("delete %s: %w", key, err)
		}
		deleted++
		deletedBytes += size
	}
	fmt.Fprintf(out, "deleted %d objects (%s of chunks)\n", deleted, formatBytes(deletedBytes))
	return nil
}

// markSweepPlan は保持世代を確定し、削除候補の manifest と chunk を返す。
func markSweepPlan(ctx context.Context, remote GCStore, keep int, cutoff time.Time) (*gcPlan, error) {
	plan := &gcPlan{
		manifests:     make(map[string]int64),
		chunks:        make(map[string]int64),
		keptPerName:   make(map[string]int),
		prunedPerName: make(map[string]int),
	}

	// 全 manifest を読む。キーから名前を取らず manifest 自身の Name を使う。
	snapObjects, err := remote.List(ctx, "snapshots/")
	if err != nil {
		return nil, err
	}
	// 同一 snapshot が新旧両形式（.json.gz と .json）で存在しうるため、
	// 物理キーは全部保持する。ID だけを鍵にして 1 つに畳むと、削除も
	// サイズ会計も片方を取りこぼす。内容は読み取り側と同じ規則で
	// .json.gz を正本とし、列挙順に依存しないようにする。
	type snapshot struct {
		keys     []string         // この ID を表す全物理キー
		sizes    map[string]int64 // キーごとのオブジェクトサイズ
		modified time.Time        // 最も新しい更新時刻（猶予判定に使う）
		manifest model.Manifest
		fromGzip bool // 採用済みの内容が .json.gz 由来か
	}
	byID := make(map[string]*snapshot)
	for _, object := range snapObjects {
		raw, err := remote.Get(ctx, object.Key)
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", object.Key, err)
		}
		gzipped := strings.HasSuffix(object.Key, ".json.gz")
		if gzipped {
			if raw, err = gunzipBytes(raw); err != nil {
				return nil, fmt.Errorf("decompress %s: %w", object.Key, err)
			}
		}
		var m model.Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", object.Key, err)
		}
		existing := byID[m.ID]
		if existing == nil {
			byID[m.ID] = &snapshot{
				keys: []string{object.Key}, sizes: map[string]int64{object.Key: object.Size},
				modified: object.LastModified, manifest: m, fromGzip: gzipped,
			}
			continue
		}
		existing.keys = append(existing.keys, object.Key)
		existing.sizes[object.Key] = object.Size
		if object.LastModified.After(existing.modified) {
			existing.modified = object.LastModified
		}
		if gzipped && !existing.fromGzip {
			existing.manifest = m
			existing.fromGzip = true
		}
	}

	// ref → parent 連鎖で保持集合と参照 chunk 集合を作る。
	refObjects, err := remote.List(ctx, "refs/")
	if err != nil {
		return nil, err
	}
	referenced := make(map[string]struct{})
	handled := make(map[string]struct{})
	markReferenced := func(m model.Manifest) {
		for _, f := range m.Files {
			for _, ch := range f.Chunks {
				if !ch.Zero {
					referenced[ch.Hash] = struct{}{}
				}
			}
		}
	}
	for _, object := range refObjects {
		if !strings.HasSuffix(object.Key, "/latest.json") {
			continue
		}
		raw, err := remote.Get(ctx, object.Key)
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", object.Key, err)
		}
		var ref model.Ref
		if err := json.Unmarshal(raw, &ref); err != nil {
			return nil, fmt.Errorf("decode %s: %w", object.Key, err)
		}
		id := ref.SnapshotID
		for generation := 0; id != ""; generation++ {
			snap, ok := byID[id]
			if !ok {
				// 連鎖の先が既に prune 済み。ここで打ち切る。
				break
			}
			handled[id] = struct{}{}
			if generation >= keep {
				// keep 超過の世代は意図的な削除対象。猶予は適用しない
				// （猶予が守るのは書き込み途中かもしれない孤児と chunk）。
				plan.prunedPerName[ref.Name]++
				for _, key := range snap.keys {
					plan.manifests[key] = snap.sizes[key]
				}
			} else {
				plan.keptPerName[ref.Name]++
				markReferenced(snap.manifest)
			}
			id = snap.manifest.ParentID
		}
	}

	// ref から到達できない孤児。書き込み途中の push（manifest 書き込み後、
	// ref 更新前）かもしれないので、猶予期間内は参照 chunk ごと残す。
	for id, snap := range byID {
		if _, done := handled[id]; done {
			continue
		}
		if snap.modified.After(cutoff) {
			markReferenced(snap.manifest)
			continue
		}
		for _, key := range snap.keys {
			plan.manifests[key] = snap.sizes[key]
		}
	}

	chunkObjects, err := remote.List(ctx, "chunks/")
	if err != nil {
		return nil, err
	}
	for _, object := range chunkObjects {
		hash := strings.TrimSuffix(strings.TrimSuffix(path.Base(object.Key), ".gz"), ".zst")
		if _, ok := referenced[hash]; ok {
			continue
		}
		if object.LastModified.After(cutoff) {
			continue
		}
		plan.chunks[object.Key] = object.Size
	}
	return plan, nil
}
