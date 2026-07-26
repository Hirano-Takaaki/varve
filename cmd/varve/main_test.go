package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hirano-Takaaki/varve/internal/app"
	"github.com/Hirano-Takaaki/varve/internal/config"
	"github.com/Hirano-Takaaki/varve/internal/store"
)

func newStoreFlags(t *testing.T, args ...string) *storeFlags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sf := &storeFlags{fs: fs}
	o := &sf.opts
	fs.StringVar(&sf.remote, "remote", "", "")
	fs.StringVar(&o.Endpoint, "endpoint", "", "")
	fs.StringVar(&o.Bucket, "bucket", "", "")
	fs.StringVar(&o.Prefix, "prefix", "", "")
	fs.StringVar(&o.Region, "region", "", "")
	fs.BoolVar(&o.PathStyle, "path-style", true, "")
	fs.BoolVar(&o.Insecure, "insecure", false, "")
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return sf
}

func noEnv(string) string { return "" }

func testConfig() *config.File {
	pathStyle := false
	return &config.File{Remotes: []config.Remote{{
		Name: "nas", Endpoint: "http://nas.local:9000", Bucket: "dev-images",
		Prefix: "team-a", Region: "eu-west-1", PathStyle: &pathStyle, Insecure: true,
	}}}
}

func TestResolveUsesSoleRemoteWhenNothingSet(t *testing.T) {
	sf := newStoreFlags(t)
	o, err := sf.resolve(testConfig(), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if o.Endpoint != "http://nas.local:9000" || o.Bucket != "dev-images" ||
		o.Prefix != "team-a" || o.Region != "eu-west-1" || o.PathStyle || !o.Insecure {
		t.Fatalf("sole remote not applied: %+v", o)
	}
}

func TestResolveNamedRemote(t *testing.T) {
	cfg := testConfig()
	cfg.Remotes = append(cfg.Remotes, config.Remote{
		Name: "cloud", Endpoint: "https://s3.example.com", Bucket: "other",
	})
	sf := newStoreFlags(t, "--remote", "cloud")
	o, err := sf.resolve(cfg, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if o.Endpoint != "https://s3.example.com" || o.Bucket != "other" || o.Insecure {
		t.Fatalf("named remote not applied: %+v", o)
	}
	if o.Prefix != "varve" {
		t.Fatalf("expected default prefix fallback, got %q", o.Prefix)
	}
}

func TestResolveUnknownRemoteFails(t *testing.T) {
	sf := newStoreFlags(t, "--remote", "missing")
	if _, err := sf.resolve(testConfig(), noEnv); err == nil {
		t.Fatal("expected unknown remote to fail")
	}
}

func TestResolveEnvOverridesRemote(t *testing.T) {
	// 環境変数で endpoint が入っている場合（フラグ既定値経由）、remote は選ばれない。
	sf := newStoreFlags(t)
	sf.opts.Endpoint = "https://from-env.example.com"
	o, err := sf.resolve(testConfig(), func(k string) string {
		if k == "VARVE_ENDPOINT" {
			return "https://from-env.example.com"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.Endpoint != "https://from-env.example.com" {
		t.Fatalf("env endpoint lost: %+v", o)
	}
	if o.Bucket != "" {
		t.Fatalf("remote must not leak fields when endpoint comes from env: %+v", o)
	}
}

func TestResolveExplicitFlagsBeatRemote(t *testing.T) {
	sf := newStoreFlags(t, "--remote", "nas", "--path-style=true", "--insecure=false", "--prefix", "override")
	o, err := sf.resolve(testConfig(), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !o.PathStyle || o.Insecure || o.Prefix != "override" {
		t.Fatalf("explicit flags overridden by remote: %+v", o)
	}
}

func TestResolveMultipleRemotesRequireExplicitChoice(t *testing.T) {
	cfg := testConfig()
	cfg.Remotes = append(cfg.Remotes, config.Remote{Name: "b", Endpoint: "https://b", Bucket: "b"})
	sf := newStoreFlags(t)
	o, err := sf.resolve(cfg, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if o.Endpoint != "" {
		t.Fatalf("no remote should be picked implicitly among several: %+v", o)
	}
}

func TestExitCodeClassification(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, exitOK},
		{flag.ErrHelp, exitOK},
		{usageErrorf("bad args"), exitUsage},
		{fmt.Errorf("pull: %w", usageErrorf("bad drive")), exitUsage},
		{&store.RequestError{Err: errors.New("connection refused")}, exitStore},
		{fmt.Errorf("download: %w", &store.RequestError{Err: errors.New("503")}), exitStore},
		{fmt.Errorf("chunk abc: %w", app.ErrIntegrity), exitIntegrity},
		{errors.New("something else"), exitError},
	}
	for _, c := range cases {
		if got := exitCode(c.err); got != c.want {
			t.Errorf("exitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

func TestHelpSubcommandPrintsUsageToStdout(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"help", "push"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Usage: varve push [options] <name> <source>") {
		t.Fatalf("help push output: %q", out.String())
	}
	if !strings.Contains(out.String(), "-chunk-size") {
		t.Fatalf("expected flag defaults in help output: %q", out.String())
	}

	out.Reset()
	if err := run(context.Background(), []string{"help", "remote"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "remote add") {
		t.Fatalf("help remote output: %q", out.String())
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{"nonsense"}, &out, &errOut)
	if exitCode(err) != exitUsage {
		t.Fatalf("expected usage exit code, got %d (%v)", exitCode(err), err)
	}
}

func TestResolveCompressionPrecedence(t *testing.T) {
	sf := &storeFlags{selected: &config.Remote{Compression: "zstd"}}
	if got := resolveCompression(sf, "gzip", noEnv); got != "gzip" {
		t.Fatalf("flag must win, got %q", got)
	}
	envZstd := func(k string) string {
		if k == "VARVE_COMPRESSION" {
			return "gzip"
		}
		return ""
	}
	if got := resolveCompression(sf, "", envZstd); got != "gzip" {
		t.Fatalf("env must beat remote, got %q", got)
	}
	if got := resolveCompression(sf, "", noEnv); got != "zstd" {
		t.Fatalf("remote setting must apply, got %q", got)
	}
	if got := resolveCompression(&storeFlags{}, "", noEnv); got != "zstd" {
		t.Fatalf("expected zstd default, got %q", got)
	}
}

func TestRemoteAddListRemove(t *testing.T) {
	t.Setenv("VARVE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	ctx := context.Background()
	var out, errOut bytes.Buffer

	err := run(ctx, []string{"remote", "add", "--bucket", "dev-images", "--prefix", "team-a", "--insecure", "nas", "http://nas.local:9000"}, &out, &errOut)
	if err != nil {
		t.Fatalf("remote add: %v (%s)", err, errOut.String())
	}
	out.Reset()
	if err := run(ctx, []string{"remote", "list"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nas") || !strings.Contains(out.String(), "insecure") {
		t.Fatalf("remote list output: %q", out.String())
	}

	// 重複追加は拒否
	if err := run(ctx, []string{"remote", "add", "--bucket", "x", "nas", "https://x"}, &out, &errOut); err == nil {
		t.Fatal("expected duplicate remote add to fail")
	}
	// HTTP は --insecure 無しで拒否
	if err := run(ctx, []string{"remote", "add", "--bucket", "x", "plain", "http://x"}, &out, &errOut); err == nil {
		t.Fatal("expected http endpoint without --insecure to fail")
	}

	if err := run(ctx, []string{"remote", "remove", "nas"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(ctx, []string{"remote", "list"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "nas") {
		t.Fatalf("remote still listed after remove: %q", out.String())
	}
}
