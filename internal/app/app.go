package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Hirano-Takaaki/varve/internal/model"
	"github.com/Hirano-Takaaki/varve/internal/store"
)

type BlobStore interface {
	Key(...string) string
	Put(context.Context, string, []byte, string) error
	Get(context.Context, string) ([]byte, error)
	Exists(context.Context, string) (bool, error)
	List(context.Context, string) ([]store.Object, error)
}

type PushOptions struct {
	Name, Source, Kind string
	Codec              string // gzip（既定）または zstd
	Concurrency        int
	ChunkSize          int64
	IgnoreLock         bool
	Progress           io.Writer
}

type PullOptions struct {
	Reference, Destination, CacheDir, DriveLetter, Seed string
	Concurrency                                         int
	Force, Mount, Trust                                 bool
	Progress                                            io.Writer
}

type uploadJob struct {
	hash string
	raw  []byte
}

// ErrIntegrity は取得データの検証失敗を表す。通信エラーと区別して
// 終了コードに反映するため、該当箇所はこれを wrap する。
var ErrIntegrity = errors.New("integrity verification failed")

func Push(ctx context.Context, remote BlobStore, o PushOptions) error {
	if err := validateName(o.Name); err != nil {
		return err
	}
	if o.Concurrency < 1 {
		return errors.New("concurrency must be at least 1")
	}
	if o.ChunkSize == 0 {
		o.ChunkSize = model.DefaultChunkSize
	}
	if o.ChunkSize < 1<<20 || o.ChunkSize > 64<<20 {
		return errors.New("chunk-size must be between 1 MiB and 64 MiB")
	}
	source, err := filepath.Abs(o.Source)
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	codec, err := NormalizeCodec(o.Codec)
	if err != nil {
		return err
	}
	kind := strings.ToLower(o.Kind)
	if kind == "auto" {
		if !info.IsDir() && strings.EqualFold(filepath.Ext(source), ".vhdx") {
			kind = "vhdx"
		} else {
			kind = "tree"
		}
	}
	if kind != "tree" && kind != "vhdx" {
		return fmt.Errorf("unsupported kind %q", kind)
	}
	if kind == "tree" && !info.IsDir() {
		return errors.New("tree source must be a directory")
	}
	if kind == "vhdx" && info.IsDir() {
		return errors.New("VHDX source must be a file")
	}
	if kind == "vhdx" {
		if err := preflightVHDX(ctx, source); err != nil {
			return err
		}
	}

	if !o.IgnoreLock {
		locked, err := remote.Exists(ctx, remote.Key(LockKey))
		if err != nil {
			return fmt.Errorf("check gc lock: %w", err)
		}
		if locked {
			// gc の sweep 中に push すると、HEAD で存在確認した chunk が
			// 直後に消される恐れがある。ロックが消えるのを待ってもらう。
			return fmt.Errorf("gc is running (%s exists); retry after it finishes, or pass --force to override", remote.Key(LockKey))
		}
	}

	previous, err := latestManifest(ctx, remote, o.Name)
	if err != nil {
		return fmt.Errorf("load latest snapshot: %w", err)
	}
	manifest := model.Manifest{
		Version: model.FormatVersion, Name: o.Name, Kind: kind, CreatedAt: time.Now().UTC(),
		ChunkSize: o.ChunkSize,
	}
	// 親世代の chunk はコーデックごと継承する。値は正規化済みコーデック。
	knownChunks := make(map[string]string)
	if previous != nil {
		manifest.ParentID = previous.ID
		for _, f := range previous.Files {
			for _, ch := range f.Chunks {
				if !ch.Zero {
					parentCodec, err := NormalizeCodec(ch.Codec)
					if err != nil {
						return fmt.Errorf("parent snapshot %s: %w", previous.ID, err)
					}
					knownChunks[ch.Hash] = parentCodec
				}
			}
		}
	}

	jobs := make(chan uploadJob, o.Concurrency*2)
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	var uploaded, remoteReused, inherited, deduplicated atomic.Int64
	// 新規 chunk のコーデックはワーカーが確定する（既存オブジェクトの再利用時は
	// そのオブジェクトのコーデック）。manifest への反映は全ワーカー終了後に行う。
	var resolvedCodecs sync.Map
	otherCodec := CodecZstd
	if codec == CodecZstd {
		otherCodec = CodecGzip
	}
	var readBuffers sync.Pool
	readBuffers.New = func() any { return make([]byte, int(o.ChunkSize)) }
	for range o.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if workCtx.Err() != nil {
					readBuffers.Put(j.raw[:cap(j.raw)])
					continue
				}
				// 希望コーデックのキーを先に、無ければもう一方も確認する。
				// 別コーデックで発行された履歴と chunk を共有するため。
				exists, err := remote.Exists(workCtx, chunkKey(remote, j.hash, codec))
				if err == nil && exists {
					resolvedCodecs.Store(j.hash, codec)
					remoteReused.Add(1)
					readBuffers.Put(j.raw[:cap(j.raw)])
					continue
				}
				if err == nil {
					var altExists bool
					altExists, err = remote.Exists(workCtx, chunkKey(remote, j.hash, otherCodec))
					if err == nil && altExists {
						resolvedCodecs.Store(j.hash, otherCodec)
						remoteReused.Add(1)
						readBuffers.Put(j.raw[:cap(j.raw)])
						continue
					}
				}
				if err == nil {
					var encoded []byte
					encoded, err = compressChunk(codec, j.raw)
					if err == nil {
						err = remote.Put(workCtx, chunkKey(remote, j.hash, codec), encoded, chunkContentType(codec))
					}
				}
				readBuffers.Put(j.raw[:cap(j.raw)])
				if err != nil {
					errOnce.Do(func() { firstErr = err; cancel() })
					continue
				}
				resolvedCodecs.Store(j.hash, codec)
				uploaded.Add(1)
			}
		}()
	}

	scheduled := make(map[string]struct{})
	var fingerprints chunkFingerprinter
	addFile := func(full, relative string, fi fs.FileInfo) error {
		f, err := os.Open(full)
		if err != nil {
			return err
		}
		defer f.Close()
		entry := model.File{Path: filepath.ToSlash(relative), Size: fi.Size()}
		if kind == "tree" {
			entry.Mode = uint32(fi.Mode().Perm())
			entry.ModTime = fi.ModTime().UTC()
		}
		for {
			if err := workCtx.Err(); err != nil {
				return err
			}
			buf := readBuffers.Get().([]byte)
			n, readErr := io.ReadFull(f, buf)
			if readErr == io.EOF {
				readBuffers.Put(buf)
				break
			}
			if readErr != nil && readErr != io.ErrUnexpectedEOF {
				readBuffers.Put(buf)
				return readErr
			}
			raw := buf[:n]
			chunkHash, zero := fingerprints.fingerprint(raw)
			chunk := model.Chunk{Hash: chunkHash, Size: int64(n), Zero: zero}
			entry.Chunks = append(entry.Chunks, chunk)
			keepBuffer := false
			if chunk.Zero {
				// A zero chunk is represented entirely by manifest metadata.
			} else if parentCodec, ok := knownChunks[chunk.Hash]; ok {
				entry.Chunks[len(entry.Chunks)-1].Codec = manifestCodec(parentCodec)
				inherited.Add(1)
			} else if _, ok := scheduled[chunk.Hash]; ok {
				deduplicated.Add(1)
			} else {
				scheduled[chunk.Hash] = struct{}{}
				select {
				case jobs <- uploadJob{chunk.Hash, raw}:
					keepBuffer = true
				case <-workCtx.Done():
					readBuffers.Put(buf)
					return workCtx.Err()
				}
			}
			if !keepBuffer {
				readBuffers.Put(buf)
			}
			if readErr == io.ErrUnexpectedEOF {
				break
			}
		}
		var chunkBytes int64
		for _, ch := range entry.Chunks {
			chunkBytes += ch.Size
		}
		if chunkBytes != fi.Size() {
			return fmt.Errorf("%s changed while it was being read (expected %d bytes, read %d)", relative, fi.Size(), chunkBytes)
		}
		manifest.Files = append(manifest.Files, entry)
		manifest.Size += fi.Size()
		return nil
	}

	if kind == "vhdx" {
		err = addFile(source, filepath.Base(source), info)
	} else {
		err = filepath.Walk(source, func(p string, fi fs.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if p == source {
				return nil
			}
			rel, err := filepath.Rel(source, p)
			if err != nil {
				return err
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("symbolic links/reparse points are not supported: %s", rel)
			}
			if fi.IsDir() {
				manifest.Files = append(manifest.Files, model.File{
					Path: filepath.ToSlash(rel) + "/", Mode: uint32(fi.Mode().Perm()), ModTime: fi.ModTime().UTC(),
				})
				return nil
			}
			if !fi.Mode().IsRegular() {
				return fmt.Errorf("unsupported file type: %s", rel)
			}
			return addFile(p, rel, fi)
		})
	}
	close(jobs)
	wg.Wait()
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if firstErr != nil {
		return firstErr
	}
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// ワーカーが確定した新規・再利用 chunk のコーデックを manifest に反映する。
	for fileIndex := range manifest.Files {
		chunks := manifest.Files[fileIndex].Chunks
		for chunkIndex := range chunks {
			if chunks[chunkIndex].Zero || chunks[chunkIndex].Codec != "" {
				continue
			}
			if value, ok := resolvedCodecs.Load(chunks[chunkIndex].Hash); ok {
				chunks[chunkIndex].Codec = manifestCodec(value.(string))
			}
		}
	}

	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	if previous != nil && sameSnapshotContent(*previous, manifest) {
		fmt.Fprintf(progress(o.Progress), "%s unchanged at %s: no snapshot or chunk objects written, %d inherited chunk references\n",
			o.Name, previous.ID, inherited.Load())
		return nil
	}
	idInput, _ := json.Marshal(manifest)
	idHash := sha256.Sum256(idInput)
	manifest.ID = manifest.CreatedAt.Format("20060102T150405Z") + "-" + hex.EncodeToString(idHash[:6])
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	ref := model.Ref{
		Name: o.Name, SnapshotID: manifest.ID, CreatedAt: manifest.CreatedAt,
		Kind: kind, ChunkSize: manifest.ChunkSize, Size: manifest.Size,
	}
	refJSON, _ := json.MarshalIndent(ref, "", "  ")
	if err := putManifest(ctx, remote, o.Name, manifest.ID, manifestJSON); err != nil {
		return err
	}
	if err := remote.Put(ctx, remote.Key("refs", o.Name, "latest.json"), refJSON, "application/json"); err != nil {
		return err
	}
	fmt.Fprintf(progress(o.Progress),
		"published %s@%s (parent %s): %s, %d files, %d uploaded, %d remote-reused, %d inherited, %d locally deduplicated chunks\n",
		o.Name, manifest.ID, parentLabel(manifest.ParentID), formatBytes(manifest.Size), len(manifest.Files),
		uploaded.Load(), remoteReused.Load(), inherited.Load(), deduplicated.Load())
	return nil
}

func Pull(ctx context.Context, remote BlobStore, o PullOptions) error {
	if o.Concurrency < 1 {
		return errors.New("concurrency must be at least 1")
	}
	name, snapshot, _ := strings.Cut(o.Reference, "@")
	if err := validateName(name); err != nil {
		return err
	}
	if snapshot == "" {
		b, err := remote.Get(ctx, remote.Key("refs", name, "latest.json"))
		if err != nil {
			return fmt.Errorf("resolve latest snapshot: %w", err)
		}
		var ref model.Ref
		if err := json.Unmarshal(b, &ref); err != nil {
			return err
		}
		snapshot = ref.SnapshotID
	}
	if strings.ContainsAny(snapshot, `/\`) || snapshot == "" {
		return errors.New("invalid snapshot ID")
	}
	b, err := getManifestBytes(ctx, remote, name, snapshot)
	if err != nil {
		return fmt.Errorf("get manifest: %w", err)
	}
	var manifest model.Manifest
	if err := json.Unmarshal(b, &manifest); err != nil {
		return err
	}
	if manifest.Version != model.FormatVersion {
		return fmt.Errorf("unsupported manifest version %d", manifest.Version)
	}
	if manifest.Name != name || manifest.ID != snapshot {
		return errors.New("manifest identity does not match reference")
	}
	if err := validateManifest(manifest); err != nil {
		return fmt.Errorf("invalid manifest: %w", err)
	}

	destination, err := safeDestination(o.Destination)
	if err != nil {
		return err
	}
	if _, err := os.Stat(destination); err == nil && !o.Force {
		return fmt.Errorf("%s already exists (use --force to replace it)", destination)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	cache, err := cachePath(o.CacheDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}

	var seed *seedPlan
	seedPath := o.Seed
	if seedPath != "" && manifest.Kind != "vhdx" {
		return errors.New("--seed is only valid for VHDX snapshots")
	}
	if seedPath == "" && o.Force && manifest.Kind == "vhdx" {
		if fi, statErr := os.Stat(destination); statErr == nil && fi.Mode().IsRegular() {
			seedPath = destination
		}
	}
	if seedPath != "" {
		seedPath, err = filepath.Abs(seedPath)
		if err != nil {
			return err
		}
		if err := preflightVHDX(ctx, seedPath); err != nil {
			return err
		}
		seed, err = indexSeed(ctx, seedPath, manifest.Files[0], manifest.ChunkSize)
		if err != nil {
			return fmt.Errorf("index seed %s: %w", seedPath, err)
		}
		if seed.reusedChunks == 0 {
			seed = nil
		}
	}

	type chunkJob struct{ chunk model.Chunk }
	unique := make(map[string]model.Chunk)
	for fileIndex, f := range manifest.Files {
		for chunkIndex, ch := range f.Chunks {
			if !ch.Zero {
				if seed != nil && fileIndex == 0 && seed.sourceOffsets[chunkIndex] >= 0 {
					continue
				}
				unique[ch.Hash] = ch
			}
		}
	}
	jobs := make(chan chunkJob)
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	var downloaded, cached atomic.Int64
	for range o.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if workCtx.Err() != nil {
					continue
				}
				hit, err := ensureChunk(workCtx, remote, cache, j.chunk)
				if err != nil {
					errOnce.Do(func() { firstErr = err; cancel() })
				} else if hit {
					cached.Add(1)
				} else {
					downloaded.Add(1)
				}
			}
		}()
	}
sendChunks:
	for _, ch := range unique {
		select {
		case jobs <- chunkJob{ch}:
		case <-workCtx.Done():
			break sendChunks
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	tempRoot, err := os.MkdirTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".varve-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempRoot)
	stage := filepath.Join(tempRoot, "content")
	if manifest.Kind == "tree" {
		if err := restoreTree(ctx, stage, cache, manifest); err != nil {
			return err
		}
	} else if manifest.Kind == "vhdx" {
		if err := os.MkdirAll(filepath.Dir(stage), 0o755); err != nil {
			return err
		}
		if seed == nil {
			if err := restoreFile(ctx, stage, cache, manifest.Files[0]); err != nil {
				return err
			}
		} else {
			if err := copyFileOptimized(seed.path, stage); err != nil {
				return fmt.Errorf("copy seed: %w", err)
			}
			if err := patchFile(ctx, stage, cache, manifest.Files[0], seed); err != nil {
				return fmt.Errorf("apply seed delta: %w", err)
			}
		}
	} else {
		return fmt.Errorf("unsupported snapshot kind %q", manifest.Kind)
	}
	if o.Force {
		if err := os.RemoveAll(destination); err != nil {
			return err
		}
	}
	if err := os.Rename(stage, destination); err != nil {
		return err
	}
	seedText := ""
	if seed != nil {
		seedText = fmt.Sprintf(", %d chunks/%s reused from seed", seed.reusedChunks, formatBytes(seed.reusedBytes))
	}
	fmt.Fprintf(progress(o.Progress), "restored %s@%s to %s: %s, %d downloaded chunks, %d cached chunks%s\n",
		name, snapshot, destination, formatBytes(manifest.Size), downloaded.Load(), cached.Load(), seedText)
	if o.Mount {
		if manifest.Kind != "vhdx" {
			return errors.New("--mount is only valid for VHDX snapshots")
		}
		letter, err := osVHDX.Mount(ctx, destination, o.DriveLetter, o.Trust)
		if err != nil {
			return err
		}
		fmt.Fprintf(progress(o.Progress), "mounted and wired at %s\n", letter)
	}
	return nil
}

func List(ctx context.Context, remote BlobStore, out io.Writer) error {
	objects, err := remote.List(ctx, "refs/")
	if err != nil {
		return err
	}
	var refs []model.Ref
	for _, object := range objects {
		if !strings.HasSuffix(object.Key, "/latest.json") {
			continue
		}
		b, err := remote.Get(ctx, object.Key)
		if err != nil {
			return err
		}
		var ref model.Ref
		if err := json.Unmarshal(b, &ref); err != nil {
			return fmt.Errorf("decode %s: %w", object.Key, err)
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	fmt.Fprintln(out, "NAME\tKIND\tSIZE\tCREATED\tSNAPSHOT")
	for _, ref := range refs {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", ref.Name, ref.Kind, formatBytes(ref.Size),
			ref.CreatedAt.Local().Format(time.RFC3339), ref.SnapshotID)
	}
	return nil
}

func History(ctx context.Context, remote BlobStore, name string, out io.Writer) error {
	if err := validateName(name); err != nil {
		return err
	}
	manifest, err := latestManifest(ctx, remote, name)
	if err != nil {
		return err
	}
	if manifest == nil {
		return fmt.Errorf("snapshot history %q does not exist", name)
	}
	fmt.Fprintln(out, "SNAPSHOT\tPARENT\tCREATED\tKIND\tCHUNK\tSIZE")
	seen := make(map[string]struct{})
	for manifest != nil {
		if _, exists := seen[manifest.ID]; exists {
			return fmt.Errorf("snapshot history contains a cycle at %s", manifest.ID)
		}
		seen[manifest.ID] = struct{}{}
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\n",
			manifest.ID, parentLabel(manifest.ParentID), manifest.CreatedAt.Local().Format(time.RFC3339),
			manifest.Kind, formatBytes(manifest.ChunkSize), formatBytes(manifest.Size))
		if manifest.ParentID == "" {
			break
		}
		parentJSON, err := getManifestBytes(ctx, remote, name, manifest.ParentID)
		if err != nil {
			if isNotFound(err) {
				// gc で prune された世代。履歴はここで途切れる。
				fmt.Fprintf(out, "%s\t(pruned)\n", manifest.ParentID)
				return nil
			}
			return fmt.Errorf("get parent snapshot %s: %w", manifest.ParentID, err)
		}
		var parent model.Manifest
		if err := json.Unmarshal(parentJSON, &parent); err != nil {
			return fmt.Errorf("decode parent snapshot %s: %w", manifest.ParentID, err)
		}
		if parent.Name != name || parent.ID != manifest.ParentID {
			return fmt.Errorf("parent snapshot %s has an invalid identity", manifest.ParentID)
		}
		if err := validateManifest(parent); err != nil {
			return fmt.Errorf("parent snapshot %s: %w", manifest.ParentID, err)
		}
		manifest = &parent
	}
	return nil
}

func ensureChunk(ctx context.Context, remote BlobStore, cache string, ch model.Chunk) (bool, error) {
	final := filepath.Join(cache, ch.Hash[:2], ch.Hash)
	if validChunk(final, ch) {
		return true, nil
	}
	chunkCodec, err := NormalizeCodec(ch.Codec)
	if err != nil {
		return false, err
	}
	encoded, err := remote.Get(ctx, chunkKey(remote, ch.Hash, chunkCodec))
	if err != nil {
		return false, err
	}
	raw, err := decompressChunk(chunkCodec, encoded, ch.Size)
	if err != nil {
		return false, err
	}
	if int64(len(raw)) != ch.Size || !hashMatches(raw, ch.Hash) {
		return false, fmt.Errorf("downloaded chunk %s: %w", ch.Hash, ErrIntegrity)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(final), "."+ch.Hash+".tmp-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return false, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return false, err
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		if !validChunk(final, ch) {
			return false, err
		}
	}
	return false, nil
}

func validChunk(path string, ch model.Chunk) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() != ch.Size {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	var actual [sha256.Size]byte
	h.Sum(actual[:0])
	return digestMatches(actual, ch.Hash)
}

func restoreTree(ctx context.Context, stage, cache string, manifest model.Manifest) error {
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	for _, f := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		clean := filepath.Clean(filepath.FromSlash(strings.TrimSuffix(f.Path, "/")))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe manifest path %q", f.Path)
		}
		target := filepath.Join(stage, clean)
		if !strings.HasPrefix(strings.ToLower(target), strings.ToLower(stage+string(filepath.Separator))) {
			return fmt.Errorf("manifest path escapes destination: %q", f.Path)
		}
		if strings.HasSuffix(f.Path, "/") {
			if err := os.MkdirAll(target, fs.FileMode(f.Mode)); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := restoreFile(ctx, target, cache, f); err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
		_ = os.Chmod(target, fs.FileMode(f.Mode))
		if !f.ModTime.IsZero() {
			_ = os.Chtimes(target, f.ModTime, f.ModTime)
		}
	}
	return nil
}

func restoreFile(ctx context.Context, target, cache string, f model.File) error {
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_ = markSparse(out)
	if err := out.Truncate(f.Size); err != nil {
		return err
	}
	var offset int64
	for _, ch := range f.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ch.Zero {
			offset += ch.Size
			continue
		}
		in, err := os.Open(filepath.Join(cache, ch.Hash[:2], ch.Hash))
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(io.NewOffsetWriter(out, offset), in, ch.Size)
		closeErr := in.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		offset += ch.Size
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return nil
}

type seedPlan struct {
	path          string
	sourceOffsets []int64
	reusedChunks  int64
	reusedBytes   int64
}

type chunkIdentity struct {
	hash string
	size int64
}

func indexSeed(ctx context.Context, path string, target model.File, chunkSize int64) (*seedPlan, error) {
	in, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("seed must be a regular file")
	}

	offsets := make(map[chunkIdentity]int64)
	var identities []chunkIdentity
	buf := make([]byte, int(chunkSize))
	var offset int64
	var fingerprints chunkFingerprinter
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, readErr := io.ReadFull(in, buf)
		if readErr == io.EOF {
			break
		}
		if readErr != nil && readErr != io.ErrUnexpectedEOF {
			return nil, readErr
		}
		raw := buf[:n]
		chunkHash, _ := fingerprints.fingerprint(raw)
		identity := chunkIdentity{hash: chunkHash, size: int64(n)}
		identities = append(identities, identity)
		if _, exists := offsets[identity]; !exists {
			offsets[identity] = offset
		}
		offset += int64(n)
		if readErr == io.ErrUnexpectedEOF {
			break
		}
	}

	plan := &seedPlan{path: path, sourceOffsets: make([]int64, len(target.Chunks))}
	for i := range plan.sourceOffsets {
		plan.sourceOffsets[i] = -1
	}
	var targetOffset int64
	for i, ch := range target.Chunks {
		identity := chunkIdentity{hash: ch.Hash, size: ch.Size}
		if i < len(identities) && identities[i] == identity {
			plan.sourceOffsets[i] = targetOffset
		} else if sourceOffset, ok := offsets[identity]; ok {
			plan.sourceOffsets[i] = sourceOffset
		}
		if plan.sourceOffsets[i] >= 0 && !ch.Zero {
			plan.reusedChunks++
			plan.reusedBytes += ch.Size
		}
		targetOffset += ch.Size
	}
	return plan, nil
}

func patchFile(ctx context.Context, target, cache string, f model.File, seed *seedPlan) error {
	out, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer out.Close()
	_ = markSparse(out)
	if err := out.Truncate(f.Size); err != nil {
		return err
	}
	seedFile, err := os.Open(seed.path)
	if err != nil {
		return err
	}
	defer seedFile.Close()

	var targetOffset int64
	for i, ch := range f.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		sourceOffset := seed.sourceOffsets[i]
		if sourceOffset == targetOffset {
			targetOffset += ch.Size
			continue
		}
		if ch.Zero {
			if err := zeroFileRange(out, targetOffset, ch.Size); err != nil {
				return err
			}
		} else if sourceOffset >= 0 {
			source := io.NewSectionReader(seedFile, sourceOffset, ch.Size)
			if _, err := io.CopyN(io.NewOffsetWriter(out, targetOffset), source, ch.Size); err != nil {
				return err
			}
		} else {
			in, err := os.Open(filepath.Join(cache, ch.Hash[:2], ch.Hash))
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(io.NewOffsetWriter(out, targetOffset), in, ch.Size)
			closeErr := in.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		targetOffset += ch.Size
	}
	return out.Sync()
}

func zeroFileRange(file *os.File, offset, size int64) error {
	if size == 0 || punchZeroRange(file, offset, size) {
		return nil
	}
	zero := make([]byte, min(size, model.DefaultChunkSize))
	writer := io.NewOffsetWriter(file, offset)
	for remaining := size; remaining > 0; {
		n := min(remaining, int64(len(zero)))
		if _, err := writer.Write(zero[:n]); err != nil {
			return err
		}
		remaining -= n
	}
	return nil
}

func latestManifest(ctx context.Context, remote BlobStore, name string) (*model.Manifest, error) {
	refKey := remote.Key("refs", name, "latest.json")
	exists, err := remote.Exists(ctx, refKey)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	refJSON, err := remote.Get(ctx, refKey)
	if err != nil {
		return nil, err
	}
	var ref model.Ref
	if err := json.Unmarshal(refJSON, &ref); err != nil {
		return nil, fmt.Errorf("decode latest reference: %w", err)
	}
	if ref.Name != name || ref.SnapshotID == "" || strings.ContainsAny(ref.SnapshotID, `/\`) {
		return nil, errors.New("latest reference has an invalid identity")
	}
	manifestJSON, err := getManifestBytes(ctx, remote, name, ref.SnapshotID)
	if err != nil {
		return nil, err
	}
	var manifest model.Manifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return nil, fmt.Errorf("decode latest manifest: %w", err)
	}
	if manifest.Name != name || manifest.ID != ref.SnapshotID {
		return nil, errors.New("latest manifest identity does not match reference")
	}
	if err := validateManifest(manifest); err != nil {
		return nil, fmt.Errorf("latest manifest: %w", err)
	}
	return &manifest, nil
}

func validateManifest(manifest model.Manifest) error {
	if manifest.Version != model.FormatVersion {
		return fmt.Errorf("unsupported format version %d", manifest.Version)
	}
	if err := validateName(manifest.Name); err != nil {
		return err
	}
	if manifest.ID == "" || strings.ContainsAny(manifest.ID, `/\`) {
		return errors.New("invalid snapshot ID")
	}
	if manifest.ParentID != "" && strings.ContainsAny(manifest.ParentID, `/\`) {
		return errors.New("invalid parent snapshot ID")
	}
	if manifest.Kind != "tree" && manifest.Kind != "vhdx" {
		return fmt.Errorf("unsupported snapshot kind %q", manifest.Kind)
	}
	if manifest.ChunkSize < 1<<20 || manifest.ChunkSize > 64<<20 {
		return fmt.Errorf("chunk size %d is outside the supported range", manifest.ChunkSize)
	}
	if manifest.Size < 0 {
		return errors.New("negative manifest size")
	}
	if manifest.Kind == "vhdx" && len(manifest.Files) != 1 {
		return errors.New("a VHDX manifest must contain exactly one file")
	}

	var totalSize int64
	seenPaths := make(map[string]struct{}, len(manifest.Files))
	zeroHashes := make(map[int64]string)
	for _, file := range manifest.Files {
		if file.Path == "" {
			return errors.New("empty manifest path")
		}
		if _, exists := seenPaths[file.Path]; exists {
			return fmt.Errorf("duplicate manifest path %q", file.Path)
		}
		seenPaths[file.Path] = struct{}{}
		if file.Size < 0 {
			return fmt.Errorf("%s has a negative size", file.Path)
		}
		if strings.HasSuffix(file.Path, "/") {
			if file.Size != 0 || len(file.Chunks) != 0 {
				return fmt.Errorf("directory %s contains file data", file.Path)
			}
			continue
		}
		var fileSize int64
		for i, ch := range file.Chunks {
			if ch.Size <= 0 || ch.Size > manifest.ChunkSize {
				return fmt.Errorf("%s chunk %d has invalid size %d", file.Path, i, ch.Size)
			}
			if i < len(file.Chunks)-1 && ch.Size != manifest.ChunkSize {
				return fmt.Errorf("%s chunk %d is not aligned to the manifest chunk size", file.Path, i)
			}
			if !validHash(ch.Hash) {
				return fmt.Errorf("%s chunk %d has an invalid SHA-256", file.Path, i)
			}
			if ch.Zero {
				zeroHash := zeroHashes[ch.Size]
				if zeroHash == "" {
					zeroHash = hashZeroes(ch.Size)
					zeroHashes[ch.Size] = zeroHash
				}
				if ch.Hash != zeroHash {
					return fmt.Errorf("%s chunk %d has inconsistent zero metadata", file.Path, i)
				}
			}
			if fileSize > maxInt64-ch.Size {
				return fmt.Errorf("%s size overflows int64", file.Path)
			}
			fileSize += ch.Size
		}
		if fileSize != file.Size {
			return fmt.Errorf("%s size is %d but chunks total %d", file.Path, file.Size, fileSize)
		}
		if totalSize > maxInt64-file.Size {
			return errors.New("manifest size overflows int64")
		}
		totalSize += file.Size
	}
	if totalSize != manifest.Size {
		return fmt.Errorf("manifest size is %d but files total %d", manifest.Size, totalSize)
	}
	return nil
}

const maxInt64 = int64(^uint64(0) >> 1)

func sameSnapshotContent(a, b model.Manifest) bool {
	if a.Kind != b.Kind || a.ChunkSize != b.ChunkSize || a.Size != b.Size || len(a.Files) != len(b.Files) {
		return false
	}
	for i := range a.Files {
		left, right := a.Files[i], b.Files[i]
		if left.Path != right.Path || left.Size != right.Size || len(left.Chunks) != len(right.Chunks) {
			return false
		}
		if a.Kind == "tree" && (left.Mode != right.Mode || !left.ModTime.Equal(right.ModTime)) {
			return false
		}
		for j := range left.Chunks {
			if left.Chunks[j] != right.Chunks[j] {
				return false
			}
		}
	}
	return true
}

func parentLabel(parent string) string {
	if parent == "" {
		return "none"
	}
	return parent
}

func cachePath(p string) (string, error) {
	if p != "" {
		return filepath.Abs(p)
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "varve", "chunks"), nil
}

func safeDestination(p string) (string, error) {
	if p == "" {
		return "", errors.New("destination is required")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	volume := filepath.VolumeName(abs)
	root := volume + string(filepath.Separator)
	if filepath.Clean(abs) == filepath.Clean(root) {
		return "", errors.New("destination cannot be a volume root")
	}
	cwd, _ := os.Getwd()
	if samePath(abs, cwd) {
		return "", errors.New("destination cannot be the current working directory")
	}
	return abs, nil
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func validateName(name string) error {
	if name == "" || len(name) > 128 {
		return fmt.Errorf("invalid snapshot name %q", name)
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return fmt.Errorf("invalid snapshot name %q (use letters, digits, dot, underscore, or hyphen)", name)
		}
	}
	return nil
}

var zeroPage [64 << 10]byte

func allZero(data []byte) bool {
	for len(data) >= len(zeroPage) {
		if !bytes.Equal(data[:len(zeroPage)], zeroPage[:]) {
			return false
		}
		data = data[len(zeroPage):]
	}
	return bytes.Equal(data, zeroPage[:len(data)])
}

type chunkFingerprinter struct {
	zeroHashes map[int]string
}

func (f *chunkFingerprinter) fingerprint(data []byte) (string, bool) {
	zero := allZero(data)
	if zero {
		if chunkHash := f.zeroHashes[len(data)]; chunkHash != "" {
			return chunkHash, true
		}
	}
	chunkHash := hash(data)
	if zero {
		if f.zeroHashes == nil {
			f.zeroHashes = make(map[int]string)
		}
		f.zeroHashes[len(data)] = chunkHash
	}
	return chunkHash, zero
}

func hash(b []byte) string {
	h := sha256.Sum256(b)
	var encoded [sha256.Size * 2]byte
	hex.Encode(encoded[:], h[:])
	return string(encoded[:])
}

func hashMatches(data []byte, expected string) bool {
	return digestMatches(sha256.Sum256(data), expected)
}

func digestMatches(digest [sha256.Size]byte, expected string) bool {
	if len(expected) != sha256.Size*2 {
		return false
	}
	const digits = "0123456789abcdef"
	for i, value := range digest {
		if expected[i*2] != digits[value>>4] || expected[i*2+1] != digits[value&0x0f] {
			return false
		}
	}
	return true
}

func validHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for i := range len(value) {
		ch := value[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}

func hashZeroes(size int64) string {
	h := sha256.New()
	var zero [32 << 10]byte
	for size > 0 {
		n := min(size, int64(len(zero)))
		_, _ = h.Write(zero[:n])
		size -= n
	}
	var encoded [sha256.Size * 2]byte
	hex.Encode(encoded[:], h.Sum(nil))
	return string(encoded[:])
}

func progress(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

func formatBytes(n int64) string {
	const unit = int64(1024)
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := unit, 0
	for v := n / unit; v >= unit && exp < 5; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
