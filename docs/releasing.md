# リリース手順

tag を push すると GitHub Actions が goreleaser を実行し、GitHub Releases に
成果物を発行する。手作業でのビルド・アップロードは不要。

## 手順

1. `main` の CI が green であることを確認する

   ```powershell
   gh run list --branch main --limit 3
   ```

2. tag を打って push する（semver、`v` 接頭辞付き）

   ```powershell
   git switch main
   git pull
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

3. リリースの完走を確認する

   ```powershell
   gh run watch
   gh release view v0.1.0
   ```

## 発行される成果物

| ファイル | 内容 |
|---|---|
| `varve_<version>_windows_amd64.zip` | `varve.exe` + README + LICENSE |
| `varve_<version>_windows_arm64.zip` | 同上（ARM64） |
| `checksums.txt` | 上記の SHA-256 |

changelog はコミット履歴から自動生成される（`docs:` / `test:` / `ci:` で始まる
コミットは除外）。

## Windows 専用にしている理由

VHDX のマウント・デタッチは Windows API（`Mount-DiskImage` / `Write-VolumeCache`）
に依存し、非 Windows ではスタブがエラーを返すだけになる。ツリー配布だけなら
動く見込みだが検証していないため、配布物は出さない。

**非 Windows のビルドが壊れていないことは CI の ubuntu ジョブで検証している**
（`.github/workflows/ci.yml`）。将来 Linux / macOS をサポートする場合は、
まず実機で pull を検証してから `.goreleaser.yaml` の `goos` に足すこと。

## バージョンの埋め込み

goreleaser が `-X main.version={{.Version}}` で `cmd/varve/main.go` の
`var version` を上書きする。`go install` でビルドされた場合はこの値が入らないため、
`runtime/debug.ReadBuildInfo()` からモジュールバージョンにフォールバックする
（`resolvedVersion()`）。どちらの経路でも `varve version` が意味のある値を返す。

## 設定を変えたとき

`.goreleaser.yaml` を編集したら、tag を打つ前に構文を検証する。

```powershell
go install github.com/goreleaser/goreleaser/v2@latest
goreleaser check
goreleaser release --snapshot --clean   # tag 無しでローカルビルドを試す
```

`dist/` は `.gitignore` 済み。
