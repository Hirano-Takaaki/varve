package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 旧形式（非圧縮 .json）の manifest だけがある store を再現する:
// 通常 push で作った .json.gz を展開して .json に置き換える。
func downgradeManifests(t *testing.T, remote *memoryStore, name string) {
	t.Helper()
	objs, err := remote.List(context.Background(), "snapshots/"+name+"/")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".json.gz") {
			continue
		}
		raw, err := gunzipBytes(remote.data[o.Key])
		if err != nil {
			t.Fatal(err)
		}
		legacy := strings.TrimSuffix(o.Key, ".gz")
		if err := remote.Put(context.Background(), legacy, raw, "application/json"); err != nil {
			t.Fatal(err)
		}
		if err := remote.Delete(context.Background(), o.Key); err != nil {
			t.Fatal(err)
		}
	}
}

// 同一 snapshot が .json.gz と .json の両方で存在する store でも、gc は
// 両方の物理オブジェクトを会計・削除しなければならない（ID キーで畳むと
// 片方が残留する）。
func TestGCDeletesBothManifestFormatsForSameSnapshot(t *testing.T) {
	ctx := context.Background()
	remote := newMemoryStore()
	source := writeSource(t, map[string][]byte{"churn.txt": []byte("v1")})
	if err := Push(ctx, remote, PushOptions{Name: "proj", Source: source, Kind: "tree", Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	// 世代 1 の manifest を旧形式でも併置する（部分移行の再現）。
	objs, err := remote.List(ctx, "snapshots/proj/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(objs))
	}
	gzKey := objs[0].Key
	raw, err := gunzipBytes(remote.data[gzKey])
	if err != nil {
		t.Fatal(err)
	}
	legacyKey := strings.TrimSuffix(gzKey, ".gz")
	if err := remote.Put(ctx, legacyKey, raw, "application/json"); err != nil {
		t.Fatal(err)
	}

	// 世代 2 を作り、keep=1 で世代 1 を prune 対象にする。
	if err := os.WriteFile(filepath.Join(source, "churn.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Push(ctx, remote, PushOptions{Name: "proj", Source: source, Kind: "tree", Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	if err := GC(ctx, remote, GCOptions{Keep: 1, Delete: true, Grace: 0, Progress: nil}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{gzKey, legacyKey} {
		if _, exists := remote.data[key]; exists {
			t.Errorf("gc left %s behind", key)
		}
	}
}

func TestPushWritesGzippedManifest(t *testing.T) {
	remote := newMemoryStore()
	source := writeSource(t, map[string][]byte{"a.txt": []byte("hello")})
	if err := Push(context.Background(), remote, PushOptions{Name: "proj", Source: source, Kind: "tree", Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	objs, err := remote.List(context.Background(), "snapshots/proj/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || !strings.HasSuffix(objs[0].Key, ".json.gz") {
		t.Fatalf("expected one .json.gz manifest, got %+v", objs)
	}
}

func TestLegacyPlainManifestStillWorks(t *testing.T) {
	ctx := context.Background()
	remote := newMemoryStore()
	content := bytes.Repeat([]byte("legacy "), 1024)
	source := writeSource(t, map[string][]byte{"data.bin": content, "churn.txt": []byte("v1")})
	if err := Push(ctx, remote, PushOptions{Name: "proj", Source: source, Kind: "tree", Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	downgradeManifests(t, remote, "proj")

	// pull: 旧形式 manifest から復元できる。
	dest := pullTo(t, remote, "proj")
	raw, err := os.ReadFile(filepath.Join(dest, "data.bin"))
	if err != nil || !bytes.Equal(raw, content) {
		t.Fatalf("legacy pull failed: %v", err)
	}

	// push: 旧形式の親から継承して次世代を作れる（新世代は .json.gz になる）。
	if err := os.WriteFile(filepath.Join(source, "churn.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Push(ctx, remote, PushOptions{Name: "proj", Source: source, Kind: "tree", Concurrency: 2}); err != nil {
		t.Fatal(err)
	}

	// history: 旧形式の親を辿れる。
	var hist bytes.Buffer
	if err := History(ctx, remote, "proj", &hist); err != nil {
		t.Fatalf("history over legacy parent: %v", err)
	}
	if strings.Contains(hist.String(), "(pruned)") {
		t.Fatalf("legacy parent misread as pruned:\n%s", hist.String())
	}

	// gc: 旧形式と新形式が混在した store を正しく mark/sweep できる。
	if err := GC(ctx, remote, GCOptions{Keep: 1, Delete: true, Grace: 0, Progress: nil}); err != nil {
		t.Fatal(err)
	}
	dest2 := pullTo(t, remote, "proj")
	raw2, err := os.ReadFile(filepath.Join(dest2, "churn.txt"))
	if err != nil || string(raw2) != "v2" {
		t.Fatalf("pull after gc over mixed manifests failed: %v", err)
	}
}
