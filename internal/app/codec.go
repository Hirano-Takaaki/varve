package app

// chunk の圧縮コーデック。content-address は圧縮前の SHA-256 のままなので、
// コーデックを切り替えても重複排除は世代・プロジェクト間で効き続ける。
// オブジェクトキーの拡張子（.gz / .zst）と manifest の codec フィールドで
// コーデックを表現し、混在した履歴も復元できる。

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	CodecGzip = "gzip"
	CodecZstd = "zstd"
)

// NormalizeCodec は空文字を gzip（後方互換の既定値）に解決する。
func NormalizeCodec(codec string) (string, error) {
	switch codec {
	case "", CodecGzip:
		return CodecGzip, nil
	case CodecZstd:
		return CodecZstd, nil
	default:
		return "", fmt.Errorf("unsupported compression %q (use %s or %s)", codec, CodecGzip, CodecZstd)
	}
}

func codecExt(codec string) string {
	if codec == CodecZstd {
		return ".zst"
	}
	return ".gz"
}

// manifestCodec は manifest に書く値。gzip は旧形式との互換のため
// 空文字（omitempty）にする。
func manifestCodec(codec string) string {
	if codec == CodecGzip {
		return ""
	}
	return codec
}

func chunkKey(remote BlobStore, hash, codec string) string {
	return remote.Key("chunks", hash[:2], hash+codecExt(codec))
}

func chunkContentType(codec string) string {
	if codec == CodecZstd {
		return "application/zstd"
	}
	return "application/gzip"
}

// zstd の Encoder / Decoder は EncodeAll / DecodeAll が並行安全なので
// プロセスで 1 つを共有する。
var (
	zstdEncoderOnce sync.Once
	zstdEncoder     *zstd.Encoder
	zstdDecoderOnce sync.Once
	zstdDecoder     *zstd.Decoder
)

func getZstdEncoder() *zstd.Encoder {
	zstdEncoderOnce.Do(func() {
		zstdEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	})
	return zstdEncoder
}

func getZstdDecoder() *zstd.Decoder {
	zstdDecoderOnce.Do(func() {
		zstdDecoder, _ = zstd.NewReader(nil)
	})
	return zstdDecoder
}

func compressChunk(codec string, raw []byte) ([]byte, error) {
	if codec == CodecZstd {
		return getZstdEncoder().EncodeAll(raw, nil), nil
	}
	var buf bytes.Buffer
	buf.Grow(len(raw) + 256)
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decompressChunk は limit+1 バイトまでで打ち切って伸長する
// （期待サイズ超過の検出は呼び出し側のサイズ検証に任せる）。
func decompressChunk(codec string, encoded []byte, limit int64) ([]byte, error) {
	if codec == CodecZstd {
		decoded, err := getZstdDecoder().DecodeAll(encoded, nil)
		if err != nil {
			return nil, err
		}
		if int64(len(decoded)) > limit+1 {
			decoded = decoded[:limit+1]
		}
		return decoded, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(zr, limit+1))
	closeErr := zr.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}
