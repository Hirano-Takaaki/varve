---
name: varve
description: varve CLI のセットアップと利用。S3 互換ストレージ経由でプロジェクトツリーや Dev Drive VHDX を世代配布する（push / pull / publish / restore / gc / remote）。接続先の登録、私有 CA を使う HTTPS ストレージへの接続、転送量の見積り、終了コードによる失敗の切り分けを扱う。varve コマンドを実行する前、または varve が失敗したときに読む。
---

# varve

内容アドレスの差分で「git が運べないもの」を配るツール。**git 管理内のソースだけを
配るなら git を使うこと**（実測で git が転送量で勝つ）。varve の領分は
ビルド成果物・`node_modules`・checkout 済みの状態・Dev Drive の VHDX。

## 最初に確認する

```powershell
varve version                 # 入っているか
varve remote list             # 接続先が登録済みか
```

`remote list` が空なら「セットアップ」へ。登録済みなら、資格情報の環境変数だけ
確認して「日常の操作」へ進む。

## セットアップ

### 1. インストール

```powershell
go install github.com/Hirano-Takaaki/varve/cmd/varve@latest
```

### 2. 接続先を登録する

```powershell
varve remote add --bucket <bucket> [--prefix <prefix>] <name> <endpoint>
```

**保存されるのは接続先だけで、資格情報は保存されない。** endpoint / bucket /
prefix / region / `--insecure` / `--path-style` / `--compression` / `--ca-cert` が
`%AppData%\varve\config.json` に入る。

remote が 1 つだけならそれが既定になる。複数あるときは各コマンドに `--remote <name>`。

### 3. 資格情報を渡す

AWS 標準の環境変数から読む。

```powershell
$env:AWS_ACCESS_KEY_ID = '...'
$env:AWS_SECRET_ACCESS_KEY = '...'
```

### 4. 私有 CA の HTTPS ストレージなら CA を渡す

LAN 内の自前ストレージ（MinIO / RustFS 等）は公的 CA の証明書を持てないので、
私有 CA の PEM を渡す。**Windows の信頼ストアに入れる必要はない。**

```powershell
varve remote add --bucket dev-images --ca-cert $HOME\.varve-ca\ca-cert.pem `
  nas https://192.168.11.16:9000
```

渡した CA はシステムの信頼ストアに**追加**される（置き換えない）ので、
私有 CA の NAS と公開 CA の AWS S3 を併用できる。

### 設定の優先順位

すべての接続設定で **フラグ > 環境変数 > remote 設定**。

| 項目 | フラグ | 環境変数 |
|---|---|---|
| エンドポイント | `--endpoint` | `VARVE_ENDPOINT` |
| バケット | `--bucket` | `VARVE_BUCKET` |
| プレフィックス | `--prefix` | `VARVE_PREFIX` |
| CA バンドル | `--ca-cert` | `VARVE_CA_CERT` |
| 圧縮 | `--compression` | `VARVE_COMPRESSION` |
| 設定ファイル | — | `VARVE_CONFIG` |

**環境変数に `VARVE_ENDPOINT` が入っていると remote 設定は一切参照されない。**
CI で上書きするための仕様。remote を使うつもりで繋がらないときは、まずこれを疑う。

## 日常の操作

### プロジェクトツリー

```powershell
varve push <name> <source-dir>          # 発行
varve pull <name> <dest-dir>            # 受け取り
varve pull <name>@<snapshot> <dest-dir> # 特定世代を復元
```

同じ `<name>` に push し続けることで差分が効く。内容が変わらない再 push は
no-op（何も書かれない）。

### Dev Drive の VHDX

**管理者ターミナルが要る**（マウント・デタッチのため）。非管理者だと
「requires an elevated terminal」で落ちる。

```powershell
varve publish <name> C:\vhdx\dev.vhdx           # 発行
varve restore --drive V <name> C:\vhdx\dev.vhdx # 受け取り
varve restore --force <name> C:\vhdx\dev.vhdx   # 既存を新世代へ更新
```

`publish` は疎通確認 → フラッシュ → デタッチ → push → 再アタッチを行い、
**push が失敗しても VHDX を必ず元の状態に戻す**。`restore` は取得に失敗したら
元の VHDX を再アタッチする。

**`push --kind vhdx` を手で回さないこと。** フラッシュを飛ばして書き込み直後に
デタッチするとキャッシュにしかないデータが失われる（`Dismount-DiskImage` は
書き戻しを保証しない）。`publish` はこれを必ず通す。

既存ファイルへの上書きは `--force` が要る。`--force` を付けると既存ファイルが
自動的に seed として走査され、変更 chunk だけがダウンロードされる。

### 確認

```powershell
varve list              # 各 name の最新世代
varve history <name>    # 最新から親世代へ。gc で消えた世代は (pruned) と出る
```

### 古い世代の回収

```powershell
varve gc                      # 既定は dry-run。回収見込みだけ表示する
varve gc --keep 10 --delete   # 実際に削除する
```

**`gc` は DeleteObject 権限を要求する唯一のコマンド。** 日常の資格情報には
その権限を持たせない運用を推奨する。

実行中は `gc.lock` が置かれ、push は既定で拒否される。急ぐなら `push --force` で
無視できるが、sweep 中に HEAD で確認した chunk が消される窓が開く。
書き込み直後のオブジェクトは `--grace`（既定 24h）の間は未参照でも消えない。

## 失敗したときの切り分け

**終了コードで原因が分かる。**

| code | 意味 | 対応 |
|---|---|---|
| 0 | 成功（`-h` / `help` を含む） | — |
| 1 | その他の実行時エラー | メッセージを読む |
| 2 | 引数・フラグの誤り | `varve help <command>` |
| 3 | S3 との通信・応答の失敗 | **リトライしてよい**。TLS 検証失敗もここ |
| 4 | 取得データの整合性検証失敗 | **リトライしても直らない**。元データから再 push |

`varve help <command>` で各コマンドのフラグ一覧が出る。

### よくある失敗

| 症状 | 原因 |
|---|---|
| `S3 endpoint must use HTTPS` | HTTP のエンドポイントに `--insecure` が無い |
| TLS の検証エラー（code 3） | 私有 CA なのに `--ca-cert` が無い、または証明書の期限切れ |
| `403` / 署名エラー | 資格情報の環境変数が未設定、または `--path-style` の不一致 |
| `requires an elevated terminal` | `publish` / `restore` を非管理者で実行した |
| `already exists` | `restore` / `pull` の宛先が存在する。`--force` が要る |
| `gc is running` | gc の sweep 中。終わるまで待つ |
| 繋がらない（remote 登録済みなのに） | `VARVE_ENDPOINT` が環境に残っている |

## 転送量の見積り

```
転送量 ≒ 変更ファイルの実バイト数 × 圧縮率
        + 全ファイル数 × 約 50 バイト（世代ごとの manifest、gzip 後）
```

圧縮率はテキスト中心で 20% 前後、圧縮済みバイナリでほぼ 100%。

**chunk はファイル単位で切るので、1 ファイルの変更で無効化されるのはその
ファイルの chunk だけ。** ツリー全体を直列化する方式（desync 等）のような
増幅は起きない。例外は 1 MiB 超のファイルの先頭・中間への挿入で、その
ファイルだけ全 chunk が無効になる（他ファイルには波及しない）。

## 注意すべき性質

- **アタッチ中の VHDX は push できない。** 不整合なスナップショットを防ぐため拒否する
- **Dev Drive の trust 指定はマシンローカル。** VHDX に付いてこないので、
  別マシンでマウントすると trusted でなくなる。`restore` は毎回 `fsutil devdrv trust` を実行する
- **chunk のハッシュは圧縮前の内容で取る。** コーデック（zstd 既定 / gzip）を
  切り替えても重複排除は効き続け、既存 chunk はそのコーデックのまま再利用される
- **manifest は `.json.gz`。** 旧形式の非圧縮 `.json` も読めるので履歴の移行は不要

## このリポジトリで作業するとき

```powershell
go vet ./...
go test ./...
gofmt -l .          # 出力が空であること（CI が要求する）
GOOS=linux go build ./...   # 非 Windows のビルドタグ側を壊していないか
```

VHDX のマウント系は Windows 専用で、非 Windows では `mount_other.go` /
`file_other.go` のスタブに切り替わる。**両方のビルドが通ることを CI が検証している**
ので、`internal/app` を触ったら Linux ビルドも確認する。

測定ハーネスは環境変数で gate されており、通常のテスト実行ではスキップされる。

```powershell
$env:MEASURE_MOCK='1'; go test ./internal/app -run TestMeasureMockRepo -v
$env:MEASURE_ALT='1';  go test ./internal/app -run TestMeasureVsAlternatives -v
$env:MEASURE_GC='1';   go test ./internal/app -run TestMeasureStoreGrowth -v
```
