# varve

Windows の開発環境とプロジェクトツリーを、内容アドレスの差分として S3 互換
ストレージ経由で配布する単一バイナリの CLI。

年縞（varve）は 1 年に 1 層ずつ積もる湖底堆積層のこと。世代が層として積み重なり、
層は不変で、増えるのは最新の 1 層だけ — このツールの構造そのものである。

外部ツール依存はない。Go 以外のビルド依存もない。

```powershell
varve remote add --bucket dev-images nas https://minio.example.com
varve push my-project D:\repos\my-project      # 発行
varve pull my-project D:\repos\my-project      # 受け取り
```

## 何のために使うか

**git で運べないものを、git のように世代管理して配る**ためのツール。

- **ビルド成果物・`node_modules`・パッケージキャッシュを含むツリー** —
  checkout 済み・ビルド済みの「すぐ使える状態」を CI や新メンバーへ配る
- **Dev Drive の VHDX まるごと** — ツールチェーンの状態ごと環境を配る
  （16.32 GB のイメージ更新が差分約 30 MB で済む実測あり）

### 使うべきでない場合

**git 管理内のソースだけを配るなら git を使うこと。** 同一ペイロードの実測で
git が転送量で初回 1.19 倍・差分 1.4 倍勝ち、さらに履歴・ブランチ・マージが
付いてくる。履歴が重いなら `git clone --filter=blob:none`（partial clone）や
[Scalar](https://git-scm.com/docs/scalar) を先に検討する。

## 仕組み

- **1 MiB 固定長**の SHA-256 content-addressed chunk（Dev Drive の管理粒度に合わせた）
- chunk はファイル単位で切るため、1 ファイルの変更で無効化されるのはその
  ファイルの chunk だけ。ツリー全体を直列化する方式のような増幅が起きない
- chunk ごとの圧縮（**zstd 既定**、`--compression gzip` も可）。ハッシュは圧縮前の
  内容で取るため、コーデックを切り替えても重複排除は効き続ける
- snapshot に親世代を記録し、直前世代の chunk は HEAD 無しで継承
- 内容が変わらない再発行は no-op（chunk / manifest / ref を一切書かない）
- 全 chunk の SHA-256 検証、作業用パスからのアトミックな配置
- VHDX 内の全ゼロ領域はアップロードせず、復元時も sparse hole として作成
- 既存 VHDX または `--seed` の一致 chunk をローカル再利用し、変更 chunk だけ取得

### 転送量の見積り

```
転送量 ≒ 変更ファイルの実バイト数 × 圧縮率
        + 全ファイル数 × 約 50 バイト（世代ごとの manifest、gzip 後）
```

圧縮率はテキスト中心で 20% 前後、圧縮済みバイナリでほぼ 100%。例外は 1 MiB 超の
ファイルの先頭・中間への挿入で、そのファイルだけ全 chunk が無効になる
（他ファイルへは波及しない）。

## インストール

```powershell
go install github.com/Hirano-Takaaki/varve/cmd/varve@latest
```

自分でビルドする場合:

```powershell
go build -trimpath -ldflags="-s -w" -o varve.exe ./cmd/varve
```

生成物は Windows amd64 で約 8 MB の単一 `.exe`。cgo は使わない。

## セットアップ

### 接続先を登録する

AWS S3、MinIO、RustFS、Cloudflare R2 など、AWS Signature Version 4 に対応する
S3 互換サービスを使える。

```powershell
varve remote add --bucket dev-images --prefix team-a nas https://minio.example.com
varve remote list
```

保存されるのは endpoint / bucket / prefix / region / `--insecure` /
`--path-style` / `--compression` のみで、**資格情報は保存しない**。設定ファイルは
`%AppData%\varve\config.json`（`VARVE_CONFIG` で変更可）。

資格情報は AWS 標準の環境変数から読む:

```powershell
$env:AWS_ACCESS_KEY_ID = '...'
$env:AWS_SECRET_ACCESS_KEY = '...'
```

remote が 1 つだけならそれが既定になる。複数あるときは `--remote nas` で選ぶ。
優先順位は **フラグ > 環境変数（`VARVE_ENDPOINT` 等）> remote 設定**で、
環境変数が入っていれば remote 設定は使われない（CI での上書き用）。

HTTP のエンドポイントは `--insecure` を明示したときだけ許可する。
virtual-hosted style が必要なサービスでは `--path-style=false` を指定する。

## 使い方

### プロジェクトツリー

```powershell
varve push my-project D:\repos\my-project
varve pull my-project D:\repos\my-project
```

`pull name@snapshot destination` で `latest` ではなく特定世代を復元できる。
既定の chunk cache は `%LocalAppData%\varve\chunks`（`--cache` で変更可）。

### Dev Drive の VHDX

**管理者権限が必要**（マウント・デタッチのため）。

```powershell
# 発行: 疎通確認 → フラッシュ → デタッチ → push → 再アタッチを 1 コマンドで行う
varve publish dev-environment C:\vhdx\dev.vhdx

# 受け取り: 取得 → マウント → Dev Drive を trusted にする
varve restore --drive V dev-environment C:\vhdx\dev.vhdx

# 既存環境を新世代へ更新（既存ファイルが seed になり差分だけ転送される）
varve restore --force dev-environment C:\vhdx\dev.vhdx
```

`publish` は push の成否によらず VHDX を必ず元の状態に戻す。`restore` は取得に
失敗した場合に元の VHDX を再アタッチする。

**手で `push --kind vhdx` を回す場合は順序を守ること。** フラッシュを飛ばして
書き込み直後にデタッチすると、キャッシュにしかないデータが失われる
（`Dismount-DiskImage` はボリュームキャッシュの書き戻しを保証しない）。
`publish` はこれを必ず通す。

### 一覧と履歴

```powershell
varve list                      # リモートにある最新世代の一覧
varve history dev-environment   # 最新から親世代へ履歴をたどる
```

### 古い世代の回収

世代を重ねると store は増え続ける。設計と実測は [docs/design-gc.md](docs/design-gc.md)。

```powershell
varve gc                        # 既定は dry-run。回収見込みを表示する
varve gc --keep 10 --delete     # 実際に削除する
```

削除中は `gc.lock` が置かれ、push は既定で拒否される（`push --force` で無視可能）。
書き込み直後のオブジェクトは `--grace`（既定 24h）の間は未参照でも消えない。
`gc` は DeleteObject 権限を要求する唯一のコマンドなので、日常の資格情報には
その権限を持たせないことを推奨する。

## オブジェクト構成

chunk は不変、manifest は世代単位で不変、ref だけが更新される。

```text
<prefix>/
  chunks/<hash-prefix>/<sha256>.zst       # または .gz（拡張子がコーデックを表す）
  snapshots/<name>/<snapshot-id>.json.gz  # parent_id で直前世代へ接続
  refs/<name>/latest.json
  gc.lock                                 # gc 実行中のみ
```

manifest は gzip 圧縮して保存する。旧形式の非圧縮 `.json` も読めるので、
既存履歴の移行は不要。

## 終了コード

| code | 意味 |
|---|---|
| 0 | 成功（`-h` / `help` を含む） |
| 1 | その他の実行時エラー |
| 2 | 引数・フラグの誤り |
| 3 | S3 エンドポイントとの通信・応答の失敗 |
| 4 | 取得データの整合性検証失敗 |

3 はリトライすべき失敗、4 は元データからの再 push が必要な失敗。

## Dev Drive の落とし穴

VHDX 配布で実際に踏んだもの。

- **trust 指定とフィルタポリシーはマシンローカル**（レジストリ）で、VHDX ファイルには
  付いてこない。別マシンでマウントすると trusted でなくなり、Defender が
  performance mode ではなく通常のリアルタイム保護で動く。`restore` は
  マウント時に毎回 `fsutil devdrv trust` を実行する
- **既定でアタッチされるフィルタは `WdFilter` のみ。** Docker や ProcMon を
  Dev Drive 上で使うなら `fsutil devdrv setfiltersallowed` で明示的に足す
- **`compact vdisk` は ReFS Dev Drive では 1 バイトも回収しない。**
  未使用領域は圧縮がよく効くので実害は小さい
- **リムーバブルディスク上の VHDX は Dev Drive に指定できない**

## 検証していないこと

- **Linux / macOS での pull。** ツリー配布は動くはずだが検証していない。
  VHDX のマウント・デタッチは Windows 専用
- **大規模チームでの並行運用。** gc の並行 push 対策は
  [docs/design-gc.md](docs/design-gc.md) の通り多層で緩和しているが、
  実運用での競合は未検証

測定の詳細と手法は測定記録リポジトリ `win-vhdx-worktree` の `docs/bench-*.md`
にある。測定ハーネスは本リポジトリに同梱されており、環境変数を付けて実行できる:

```powershell
$env:MEASURE_MOCK='1'; go test ./internal/app -run TestMeasureMockRepo -v
$env:MEASURE_ALT='1';  go test ./internal/app -run TestMeasureVsAlternatives -v
$env:MEASURE_GC='1';   go test ./internal/app -run TestMeasureStoreGrowth -v
```

## ライセンス

MIT。[LICENSE](LICENSE) を参照。
