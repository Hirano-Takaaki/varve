package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeVHDX struct {
	attached        bool
	letter          string
	detachCalls     int
	mountCalls      int
	mountErr        error
	lastMountLetter string
}

func (f *fakeVHDX) Detach(_ context.Context, _ string) (bool, string, error) {
	f.detachCalls++
	was := f.attached
	f.attached = false
	return was, f.letter, nil
}

func (f *fakeVHDX) Mount(_ context.Context, _, letter string, _ bool) (string, error) {
	f.mountCalls++
	f.lastMountLetter = letter
	if f.mountErr != nil {
		return "", f.mountErr
	}
	f.attached = true
	if letter == "" {
		letter = "D"
	}
	return letter + ":", nil
}

func swapVHDX(t *testing.T, fake *fakeVHDX) {
	t.Helper()
	previous := osVHDX
	osVHDX = fake
	t.Cleanup(func() { osVHDX = previous })
}

func writeFakeImage(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env.vhdx")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xab}, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPublishAbortsBeforeDetachWhenStoreUnreachable(t *testing.T) {
	fake := &fakeVHDX{attached: true, letter: "D"}
	swapVHDX(t, fake)
	remote := newMemoryStore()
	remote.failList = errors.New("connection refused")

	err := Publish(context.Background(), remote, PublishOptions{
		Name: "env", Path: writeFakeImage(t), Concurrency: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "connectivity") {
		t.Fatalf("expected connectivity error, got %v", err)
	}
	if fake.detachCalls != 0 {
		t.Fatalf("detach must not run when the store is unreachable (called %d times)", fake.detachCalls)
	}
}

func TestPublishDetachesPushesAndRemounts(t *testing.T) {
	fake := &fakeVHDX{attached: true, letter: "D"}
	swapVHDX(t, fake)
	remote := newMemoryStore()

	var out bytes.Buffer
	if err := Publish(context.Background(), remote, PublishOptions{
		Name: "env", Path: writeFakeImage(t), Concurrency: 1, Trust: true, Progress: &out,
	}); err != nil {
		t.Fatal(err)
	}
	if fake.detachCalls != 1 || fake.mountCalls != 1 {
		t.Fatalf("detach=%d mount=%d, want 1/1", fake.detachCalls, fake.mountCalls)
	}
	if fake.lastMountLetter != "D" {
		t.Fatalf("remount letter = %q, want previous letter D", fake.lastMountLetter)
	}
	if got := countPrefix(t, remote, "snapshots/env/"); got != 1 {
		t.Fatalf("expected 1 snapshot, got %d", got)
	}
}

func TestPublishRemountsEvenWhenPushFails(t *testing.T) {
	fake := &fakeVHDX{attached: true, letter: "E"}
	swapVHDX(t, fake)
	remote := newMemoryStore()
	// gc.lock を置いて push を確実に失敗させる。
	if err := remote.Put(context.Background(), LockKey, []byte(`{}`), "application/json"); err != nil {
		t.Fatal(err)
	}

	err := Publish(context.Background(), remote, PublishOptions{
		Name: "env", Path: writeFakeImage(t), Concurrency: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "gc is running") {
		t.Fatalf("expected push failure to surface, got %v", err)
	}
	if fake.mountCalls != 1 {
		t.Fatalf("VHDX must be re-attached after a failed push (mount calls %d)", fake.mountCalls)
	}
}

func TestPublishSkipRemount(t *testing.T) {
	fake := &fakeVHDX{attached: true, letter: "D"}
	swapVHDX(t, fake)
	remote := newMemoryStore()
	if err := Publish(context.Background(), remote, PublishOptions{
		Name: "env", Path: writeFakeImage(t), Concurrency: 1, SkipRemount: true,
	}); err != nil {
		t.Fatal(err)
	}
	if fake.mountCalls != 0 {
		t.Fatalf("SkipRemount must not mount (mount calls %d)", fake.mountCalls)
	}
}

func TestPublishFailsWhenRemountFails(t *testing.T) {
	fake := &fakeVHDX{attached: true, letter: "D", mountErr: errors.New("mount blew up")}
	swapVHDX(t, fake)
	remote := newMemoryStore()
	err := Publish(context.Background(), remote, PublishOptions{
		Name: "env", Path: writeFakeImage(t), Concurrency: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "re-attach failed") {
		t.Fatalf("expected re-attach failure to be reported, got %v", err)
	}
	// 発行自体は成功している。
	if got := countPrefix(t, remote, "snapshots/env/"); got != 1 {
		t.Fatalf("expected the push to have succeeded, got %d snapshots", got)
	}
}

func TestRestoreRequiresForceForExistingFile(t *testing.T) {
	fake := &fakeVHDX{attached: true, letter: "D"}
	swapVHDX(t, fake)
	err := Restore(context.Background(), newMemoryStore(), RestoreOptions{
		Reference: "env", Path: writeFakeImage(t), Concurrency: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("expected force guidance, got %v", err)
	}
	if fake.detachCalls != 0 {
		t.Fatal("must not detach before the force check")
	}
}

func TestRestorePullsAndMounts(t *testing.T) {
	fake := &fakeVHDX{attached: false}
	swapVHDX(t, fake)
	remote := newMemoryStore()
	image := writeFakeImage(t)
	if err := Publish(context.Background(), remote, PublishOptions{
		Name: "env", Path: image, Concurrency: 1,
	}); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "restored.vhdx")
	var out bytes.Buffer
	if err := Restore(context.Background(), remote, RestoreOptions{
		Reference: "env", Path: dest, Concurrency: 1, Trust: true,
		CacheDir: filepath.Join(t.TempDir(), "cache"), Progress: &out,
	}); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(image)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, restored) {
		t.Fatal("restored VHDX differs from the published one")
	}
	if fake.mountCalls == 0 {
		t.Fatal("restore must mount the restored VHDX")
	}
}

func TestRestoreRemountsPreviousImageOnFailure(t *testing.T) {
	fake := &fakeVHDX{attached: true, letter: "V"}
	swapVHDX(t, fake)
	remote := newMemoryStore() // 参照が存在しないので pull は失敗する

	err := Restore(context.Background(), remote, RestoreOptions{
		Reference: "missing", Path: writeFakeImage(t), Concurrency: 1, Force: true,
	})
	if err == nil {
		t.Fatal("expected restore to fail for a missing reference")
	}
	if fake.detachCalls != 1 || fake.mountCalls != 1 {
		t.Fatalf("detach=%d mount=%d, want 1/1 (previous image re-attached)", fake.detachCalls, fake.mountCalls)
	}
}
