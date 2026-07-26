package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Hirano-Takaaki/varve/internal/app"
	"github.com/Hirano-Takaaki/varve/internal/config"
	"github.com/Hirano-Takaaki/varve/internal/model"
	"github.com/Hirano-Takaaki/varve/internal/store"
)

var version = "dev"

// 終了コードの規約。スクリプトからの失敗分類に使う。
const (
	exitOK        = 0
	exitError     = 1 // その他の実行時エラー
	exitUsage     = 2 // 引数・フラグの誤り
	exitStore     = 3 // S3 エンドポイントとの通信・応答の失敗
	exitIntegrity = 4 // 取得データの検証失敗
)

// usageError は引数の誤りを表し、終了コード 2 に対応付ける。
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usageErrorf(format string, a ...any) error {
	return usageError{fmt.Errorf(format, a...)}
}

func exitCode(err error) int {
	var ue usageError
	var re *store.RequestError
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		// -h / --help は usage を表示済み。エラーではない。
		return exitOK
	case errors.As(err, &ue):
		return exitUsage
	case errors.Is(err, app.ErrIntegrity):
		return exitIntegrity
	case errors.As(err, &re):
		return exitStore
	default:
		return exitError
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	os.Exit(exitCode(err))
}

func resolvedVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

// resolveCompression は push の圧縮コーデックを フラグ > 環境変数 >
// remote 設定 > 既定 zstd の順で決める。既定は A1 の実測
// （測定記録リポジトリ win-vhdx-worktree の docs/bench-mock-repo.md: 同等以上の速度で約 6% 小さい）に基づく。
// 既存の gzip 履歴とはクロスコーデック重複排除で共存する。
func resolveCompression(sf *storeFlags, flagValue string, envLookup func(string) string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := envLookup("VARVE_COMPRESSION"); v != "" {
		return v
	}
	if sf.selected != nil && sf.selected.Compression != "" {
		return sf.selected.Compression
	}
	return app.CodecZstd
}

// wrapParseError は flag 解析の失敗を usage エラーに分類する。
// -h による flag.ErrHelp はそのまま通す（エラーではないため）。
func wrapParseError(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return err
	}
	return usageError{err}
}

// setUsage はサブコマンドの構文行付きの usage を設定する。
// help <command> と -h の両方がここを通る。
func setUsage(fs *flag.FlagSet, syntax string) {
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s\n\nOptions:\n", syntax)
		fs.PrintDefaults()
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return usageErrorf("no command given")
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Fprintf(stdout, "varve %s (%s/%s)\n", resolvedVersion(), runtime.GOOS, runtime.GOARCH)
		return nil
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) >= 2 {
			// サブコマンドに -h を渡し、usage の出力先を stdout に差し替える。
			err := run(ctx, []string{args[1], "-h"}, stdout, stdout)
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		usage(stdout)
		return nil
	}

	common := func(fs *flag.FlagSet) *storeFlags {
		sf := &storeFlags{fs: fs}
		o := &sf.opts
		fs.StringVar(&sf.remote, "remote", "", "named remote from the config file")
		fs.StringVar(&o.Endpoint, "endpoint", env("VARVE_ENDPOINT", ""), "S3 endpoint URL")
		fs.StringVar(&o.Bucket, "bucket", env("VARVE_BUCKET", ""), "S3 bucket")
		fs.StringVar(&o.Prefix, "prefix", env("VARVE_PREFIX", ""), "object key prefix")
		fs.StringVar(&o.Region, "region", env("AWS_REGION", env("AWS_DEFAULT_REGION", "")), "signing region")
		fs.StringVar(&o.AccessKey, "access-key", env("AWS_ACCESS_KEY_ID", ""), "S3 access key")
		fs.StringVar(&o.SecretKey, "secret-key", env("AWS_SECRET_ACCESS_KEY", ""), "S3 secret key")
		fs.StringVar(&o.SessionToken, "session-token", env("AWS_SESSION_TOKEN", ""), "temporary credential token")
		fs.BoolVar(&o.PathStyle, "path-style", envBool("VARVE_PATH_STYLE", true), "use path-style S3 URLs")
		fs.BoolVar(&o.Insecure, "insecure", false, "allow plain HTTP endpoint")
		fs.StringVar(&o.CACertFile, "ca-cert", env("VARVE_CA_CERT", ""), "PEM bundle trusted in addition to the system store")
		return sf
	}
	newClient := func(sf *storeFlags) (*store.Client, error) {
		configPath, err := config.DefaultPath()
		if err != nil {
			return nil, err
		}
		cfg, err := config.Load(configPath)
		if err != nil {
			return nil, err
		}
		o, err := sf.resolve(cfg, os.Getenv)
		if err != nil {
			return nil, err
		}
		return store.New(o)
	}

	switch args[0] {
	case "push":
		fs := flag.NewFlagSet("push", flag.ContinueOnError)
		fs.SetOutput(stderr)
		setUsage(fs, "varve push [options] <name> <source>")
		o := common(fs)
		concurrency := fs.Int("concurrency", 8, "parallel uploads")
		chunkSize := fs.Int64("chunk-size", model.DefaultChunkSize, "fixed chunk size in bytes; 1 MiB keeps VHDX history aligned")
		kind := fs.String("kind", "auto", "snapshot kind: auto, tree, or vhdx")
		force := fs.Bool("force", false, "push even while a gc lock is present")
		compression := fs.String("compression", "", "chunk compression: gzip or zstd (default: remote setting, then zstd)")
		if err := fs.Parse(args[1:]); err != nil {
			return wrapParseError(err)
		}
		if fs.NArg() != 2 {
			return usageErrorf("usage: varve push [options] <name> <source>")
		}
		client, err := newClient(o)
		if err != nil {
			return err
		}
		return app.Push(ctx, client, app.PushOptions{
			Name: fs.Arg(0), Source: fs.Arg(1), Kind: *kind,
			Codec:       resolveCompression(o, *compression, os.Getenv),
			Concurrency: *concurrency, ChunkSize: *chunkSize, IgnoreLock: *force, Progress: stderr,
		})

	case "pull":
		fs := flag.NewFlagSet("pull", flag.ContinueOnError)
		fs.SetOutput(stderr)
		setUsage(fs, "varve pull [options] <name[@snapshot]> <destination>")
		o := common(fs)
		concurrency := fs.Int("concurrency", 12, "parallel downloads")
		cache := fs.String("cache", "", "local chunk cache directory")
		seed := fs.String("seed", "", "previous VHDX used as a local fixed-chunk seed")
		force := fs.Bool("force", false, "replace an existing destination")
		mount := fs.Bool("mount", false, "mount a restored VHDX and trust its Dev Drive")
		drive := fs.String("drive", "", "drive letter used with --mount")
		noTrust := fs.Bool("no-trust", false, "do not mark a mounted Dev Drive trusted")
		if err := fs.Parse(args[1:]); err != nil {
			return wrapParseError(err)
		}
		if fs.NArg() != 2 {
			return usageErrorf("usage: varve pull [options] <name[@snapshot]> <destination>")
		}
		// 復元を始める前に弾く。マウント直前まで遅らせると、大きな VHDX を
		// ダウンロードし終えてから不正なドライブレターで失敗する。
		driveLetter, err := app.NormalizeDriveLetter(*drive)
		if err != nil {
			return usageError{err}
		}
		client, err := newClient(o)
		if err != nil {
			return err
		}
		return app.Pull(ctx, client, app.PullOptions{
			Reference: fs.Arg(0), Destination: fs.Arg(1), CacheDir: *cache,
			Concurrency: *concurrency, Force: *force, Mount: *mount,
			DriveLetter: driveLetter, Trust: !*noTrust, Seed: *seed, Progress: stderr,
		})

	case "publish":
		fs := flag.NewFlagSet("publish", flag.ContinueOnError)
		fs.SetOutput(stderr)
		setUsage(fs, "varve publish [options] <name> <vhdx-path>")
		o := common(fs)
		concurrency := fs.Int("concurrency", 8, "parallel uploads")
		chunkSize := fs.Int64("chunk-size", model.DefaultChunkSize, "fixed chunk size in bytes; 1 MiB keeps VHDX history aligned")
		drive := fs.String("drive", "", "drive letter used when re-attaching (default: the previous letter)")
		skipRemount := fs.Bool("skip-remount", false, "do not re-attach after publishing")
		noTrust := fs.Bool("no-trust", false, "do not mark the re-attached Dev Drive trusted")
		force := fs.Bool("force", false, "publish even while a gc lock is present")
		compression := fs.String("compression", "", "chunk compression: gzip or zstd (default: remote setting, then zstd)")
		if err := fs.Parse(args[1:]); err != nil {
			return wrapParseError(err)
		}
		if fs.NArg() != 2 {
			return usageErrorf("usage: varve publish [options] <name> <vhdx-path>")
		}
		driveLetter, err := app.NormalizeDriveLetter(*drive)
		if err != nil {
			return usageError{err}
		}
		client, err := newClient(o)
		if err != nil {
			return err
		}
		return app.Publish(ctx, client, app.PublishOptions{
			Name: fs.Arg(0), Path: fs.Arg(1), DriveLetter: driveLetter,
			Codec:       resolveCompression(o, *compression, os.Getenv),
			Concurrency: *concurrency, ChunkSize: *chunkSize, IgnoreLock: *force,
			SkipRemount: *skipRemount, Trust: !*noTrust, Progress: stderr,
		})

	case "restore":
		fs := flag.NewFlagSet("restore", flag.ContinueOnError)
		fs.SetOutput(stderr)
		setUsage(fs, "varve restore [options] <name[@snapshot]> <vhdx-path>")
		o := common(fs)
		concurrency := fs.Int("concurrency", 12, "parallel downloads")
		cache := fs.String("cache", "", "local chunk cache directory")
		seed := fs.String("seed", "", "previous VHDX used as a local fixed-chunk seed")
		force := fs.Bool("force", false, "update an existing VHDX (it is scanned as a seed)")
		drive := fs.String("drive", "", "drive letter for the mounted VHDX")
		noTrust := fs.Bool("no-trust", false, "do not mark the mounted Dev Drive trusted")
		if err := fs.Parse(args[1:]); err != nil {
			return wrapParseError(err)
		}
		if fs.NArg() != 2 {
			return usageErrorf("usage: varve restore [options] <name[@snapshot]> <vhdx-path>")
		}
		driveLetter, err := app.NormalizeDriveLetter(*drive)
		if err != nil {
			return usageError{err}
		}
		client, err := newClient(o)
		if err != nil {
			return err
		}
		return app.Restore(ctx, client, app.RestoreOptions{
			Reference: fs.Arg(0), Path: fs.Arg(1), DriveLetter: driveLetter,
			Seed: *seed, CacheDir: *cache, Concurrency: *concurrency,
			Force: *force, Trust: !*noTrust, Progress: stderr,
		})

	case "gc":
		fs := flag.NewFlagSet("gc", flag.ContinueOnError)
		fs.SetOutput(stderr)
		setUsage(fs, "varve gc [options]")
		o := common(fs)
		keep := fs.Int("keep", 10, "generations to keep per name")
		del := fs.Bool("delete", false, "actually delete; the default is a dry-run report")
		grace := fs.Duration("grace", 24*time.Hour, "protect unreferenced objects newer than this")
		if err := fs.Parse(args[1:]); err != nil {
			return wrapParseError(err)
		}
		if fs.NArg() != 0 {
			return usageErrorf("usage: varve gc [options]")
		}
		client, err := newClient(o)
		if err != nil {
			return err
		}
		return app.GC(ctx, client, app.GCOptions{
			Keep: *keep, Delete: *del, Grace: *grace, Progress: stdout,
		})

	case "remote":
		return runRemote(args[1:], stdout, stderr)

	case "list":
		fs := flag.NewFlagSet("list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		setUsage(fs, "varve list [options]")
		o := common(fs)
		if err := fs.Parse(args[1:]); err != nil {
			return wrapParseError(err)
		}
		if fs.NArg() != 0 {
			return usageErrorf("usage: varve list [options]")
		}
		client, err := newClient(o)
		if err != nil {
			return err
		}
		return app.List(ctx, client, stdout)
	case "history":
		fs := flag.NewFlagSet("history", flag.ContinueOnError)
		fs.SetOutput(stderr)
		setUsage(fs, "varve history [options] <name>")
		o := common(fs)
		if err := fs.Parse(args[1:]); err != nil {
			return wrapParseError(err)
		}
		if fs.NArg() != 1 {
			return usageErrorf("usage: varve history [options] <name>")
		}
		client, err := newClient(o)
		if err != nil {
			return err
		}
		return app.History(ctx, client, fs.Arg(0), stdout)
	default:
		return usageErrorf("unknown command %q (try \"varve help\")", args[0])
	}
}

// storeFlags は S3 接続フラグと、その解決に必要な文脈（どのフラグが
// 明示されたか）を束ねる。優先順位は フラグ > 環境変数 > remote 設定。
type storeFlags struct {
	fs       *flag.FlagSet
	remote   string
	opts     store.Options
	selected *config.Remote // resolve で選ばれた remote（無ければ nil）
}

func (sf *storeFlags) resolve(cfg *config.File, envLookup func(string) string) (store.Options, error) {
	o := sf.opts
	set := make(map[string]bool)
	sf.fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	var r *config.Remote
	if sf.remote != "" {
		if r = cfg.Get(sf.remote); r == nil {
			return o, fmt.Errorf("remote %q not found (add it with \"varve remote add\")", sf.remote)
		}
	} else if o.Endpoint == "" {
		// endpoint がフラグにも環境変数にも無いときだけ、唯一の remote を既定にする。
		// 既存の環境変数運用の挙動を変えないための条件。
		r = cfg.Sole()
	}
	sf.selected = r
	if r != nil {
		if o.Endpoint == "" {
			o.Endpoint = r.Endpoint
		}
		if o.Bucket == "" {
			o.Bucket = r.Bucket
		}
		if o.Prefix == "" {
			o.Prefix = r.Prefix
		}
		if o.Region == "" {
			o.Region = r.Region
		}
		if !set["path-style"] && envLookup("VARVE_PATH_STYLE") == "" && r.PathStyle != nil {
			o.PathStyle = *r.PathStyle
		}
		if !set["insecure"] && r.Insecure {
			o.Insecure = true
		}
		if o.CACertFile == "" {
			o.CACertFile = r.CACert
		}
	}
	if o.Prefix == "" {
		o.Prefix = "varve"
	}
	return o, nil
}

func runRemote(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageErrorf("usage: varve remote <add|list|remove> ...")
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stdout, `Usage:
  varve remote add [options] --bucket <bucket> <name> <endpoint>
  varve remote list
  varve remote remove <name>`)
		return flag.ErrHelp
	}
	configPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("remote add", flag.ContinueOnError)
		fs.SetOutput(stderr)
		bucket := fs.String("bucket", "", "S3 bucket (required)")
		prefix := fs.String("prefix", "", "object key prefix")
		region := fs.String("region", "", "signing region")
		pathStyle := fs.Bool("path-style", true, "use path-style S3 URLs")
		insecure := fs.Bool("insecure", false, "allow plain HTTP endpoint")
		compression := fs.String("compression", "", "default chunk compression for push: gzip or zstd")
		caCert := fs.String("ca-cert", "", "PEM bundle trusted in addition to the system store")
		setUsage(fs, "varve remote add [options] --bucket <bucket> <name> <endpoint>")
		if err := fs.Parse(args[1:]); err != nil {
			return wrapParseError(err)
		}
		if fs.NArg() != 2 {
			return usageErrorf("usage: varve remote add [options] --bucket <bucket> <name> <endpoint>")
		}
		name, endpoint := fs.Arg(0), fs.Arg(1)
		if name == "" || strings.ContainsAny(name, `/\ `) {
			return usageErrorf("invalid remote name %q", name)
		}
		if *bucket == "" {
			return usageErrorf("--bucket is required")
		}
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" {
			return usageErrorf("invalid endpoint %q", endpoint)
		}
		if u.Scheme != "https" && !(*insecure && u.Scheme == "http") {
			return usageErrorf("endpoint must use HTTPS (add --insecure to explicitly allow HTTP)")
		}
		if *compression != "" {
			if _, err := app.NormalizeCodec(*compression); err != nil {
				return usageError{err}
			}
		}
		r := config.Remote{
			Name: name, Endpoint: endpoint, Bucket: *bucket,
			Prefix: *prefix, Region: *region, Insecure: *insecure,
			Compression: *compression, CACert: *caCert,
		}
		explicitPathStyle := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "path-style" {
				explicitPathStyle = true
			}
		})
		if explicitPathStyle {
			r.PathStyle = pathStyle
		}
		if err := cfg.Add(r); err != nil {
			return err
		}
		if err := config.Save(configPath, cfg); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "added remote %s (%s, bucket %s)\n", name, endpoint, *bucket)
		return nil

	case "list":
		if len(args) != 1 {
			return usageErrorf("usage: varve remote list")
		}
		for _, r := range cfg.Remotes {
			extra := ""
			if r.Prefix != "" {
				extra += " prefix=" + r.Prefix
			}
			if r.Insecure {
				extra += " insecure"
			}
			if r.PathStyle != nil && !*r.PathStyle {
				extra += " virtual-hosted"
			}
			if r.Compression != "" {
				extra += " compression=" + r.Compression
			}
			if r.CACert != "" {
				extra += " ca-cert=" + r.CACert
			}
			fmt.Fprintf(stdout, "%s\t%s\tbucket=%s%s\n", r.Name, r.Endpoint, r.Bucket, extra)
		}
		return nil

	case "remove":
		if len(args) != 2 {
			return usageErrorf("usage: varve remote remove <name>")
		}
		if err := cfg.Remove(args[1]); err != nil {
			return err
		}
		if err := config.Save(configPath, cfg); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed remote %s\n", args[1])
		return nil

	default:
		return usageErrorf("unknown remote subcommand %q (try add, list, or remove)", args[0])
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `varve - content-addressed project/VHDX distribution over S3

Usage:
  varve push [options] <name> <source>
  varve pull [options] <name[@snapshot]> <destination>
  varve publish [options] <name> <vhdx-path>
  varve restore [options] <name[@snapshot]> <vhdx-path>
  varve list [options]
  varve history [options] <name>
  varve gc [options]
  varve remote add [options] --bucket <bucket> <name> <endpoint>
  varve remote list
  varve remote remove <name>
  varve help [command]
  varve version

Exit codes:
  0 success   1 error   2 usage   3 storage/network   4 integrity

Connection settings resolve in this order: command-line flags, then
VARVE_ENDPOINT / VARVE_BUCKET / VARVE_PREFIX environment variables,
then the remote selected with --remote (or the sole configured remote).
Credentials are never stored in the config file; supply them with the
standard AWS credential environment variables.
Run "varve <command> -h" for command-specific options.`)
}
