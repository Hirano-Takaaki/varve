package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileReturnsEmpty(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "none", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Remotes) != 0 {
		t.Fatalf("expected empty config, got %d remotes", len(f.Remotes))
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	pathStyle := false
	f := &File{Remotes: []Remote{{
		Name: "nas", Endpoint: "https://minio.example.com", Bucket: "dev-images",
		Prefix: "team-a", Region: "us-east-1", PathStyle: &pathStyle, Insecure: true,
	}}}
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r := loaded.Get("nas")
	if r == nil {
		t.Fatal("remote nas not found after round trip")
	}
	if r.Endpoint != "https://minio.example.com" || r.Bucket != "dev-images" ||
		r.Prefix != "team-a" || !r.Insecure || r.PathStyle == nil || *r.PathStyle {
		t.Fatalf("round trip mismatch: %+v", r)
	}
}

func TestAddDuplicateFails(t *testing.T) {
	f := &File{}
	if err := f.Add(Remote{Name: "a", Endpoint: "https://x", Bucket: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Add(Remote{Name: "a", Endpoint: "https://y", Bucket: "b"}); err == nil {
		t.Fatal("expected duplicate add to fail")
	}
}

func TestRemove(t *testing.T) {
	f := &File{Remotes: []Remote{{Name: "a"}, {Name: "b"}}}
	if err := f.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if f.Get("a") != nil || f.Get("b") == nil {
		t.Fatalf("unexpected remotes after remove: %+v", f.Remotes)
	}
	if err := f.Remove("missing"); err == nil {
		t.Fatal("expected removing unknown remote to fail")
	}
}

func TestSole(t *testing.T) {
	f := &File{Remotes: []Remote{{Name: "only"}}}
	if f.Sole() == nil || f.Sole().Name != "only" {
		t.Fatal("expected sole remote")
	}
	f.Remotes = append(f.Remotes, Remote{Name: "second"})
	if f.Sole() != nil {
		t.Fatal("expected no sole remote when two exist")
	}
}

func TestNewerVersionRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version": 99, "remotes": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected newer version to be rejected")
	}
}

func TestDefaultPathHonorsEnvOverride(t *testing.T) {
	t.Setenv("VARVE_CONFIG", `C:\custom\config.json`)
	p, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if p != `C:\custom\config.json` {
		t.Fatalf("unexpected path %q", p)
	}
}
