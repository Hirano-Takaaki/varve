# A3: chunk store の肥大の実測と gc の要件

計画: `docs/plan-cli-distribution.md` の A3。成果物は要件文書であり、実装は B3 で行う。

## 実測: store は世代に比例して増え続ける

### 合成データでの測定

`internal/app/gc_growth_measure_test.go`（`MEASURE_GC=1` でのみ実行）で、
in-memory store に 500 ファイル × 64 KiB（乱数 = 非圧縮性）のツリーを、
世代ごとに 5% のファイルを書き換えながら 10 世代 push した。

| 世代 | store 合計 | 増分 |
|---|---|---|
| 1（初回） | 32.9 MB | — |
| 2〜10（各） | — | **+1.7 MB / 世代（ほぼ一定）** |
| 10 | 48.6 MB | 初回比 +47% |

増分の内訳は「変更ファイル 25 個分の chunk（約 1.57 MB）+ manifest（約 148 KB）」。
**増加は完全に線形で、自然に頭打ちになる要素はない。**

この時点で直近 keep 世代だけを残す mark-and-sweep を計算すると:

| keep | 回収可能 |
|---|---|
| 1 | 30.4%（14.3 MB / 218 chunks） |
| 3 | 23.5% |
| 5 | 16.7% |

### 実運用の数字への外挿

README 記載の実測（VHDX 初回 2.56 GB + 世代あたり約 30 MB、ツリー初回 116 MB +
世代あたり 16〜23 MB）を 1 日 1 世代 × 1 年（250 世代）に外挿すると:

| 対象 | 初回 | 250 世代後 | 倍率 |
|---|---|---|---|
| VHDX (16.32 GB) | 2.56 GB | 約 10.1 GB | 3.9 倍 |
| ツリー (250 MB) | 116 MB | 約 5.1 GB | **44 倍** |

**gc が最も切実なのはツリー発行**。初回が小さいぶん、差分の堆積が支配的になる。

### manifest 自体も無視できない

manifest は当初、非圧縮 JSON で **ファイル 1 個あたり約 300 バイト × 全ファイル数**が
世代ごとに丸ごと書かれていた。20,040 ファイルの実リポジトリなら世代あたり約 6 MB、
250 世代で約 1.5 GB。

**この検討事項は B7 で実装した**（`.json.gz` として保存、旧 `.json` も読める
後方互換）。実測で世代あたりの manifest は約 83% 減（1.4 MB → 0.24 MB、
`docs/bench-vs-alternatives.md`）。残る分は chunk と同様に世代の削除で回収する。

## メタデータの充足性: mark-and-sweep は現行形式で可能

必要な情報はすべて揃っている。

- `snapshots/<name>/<id>.json` が全 chunk のハッシュを `files[].chunks[].hash` に列挙する
- `refs/<name>/latest.json` → `parent_id` の連鎖で世代の新旧を決定できる
- `List` で `snapshots/` と `chunks/` の全列挙ができる（ページング実装済み）

不足はストア操作のみ:

- **`store.Client` に Delete（DeleteObject）が無い。** B3 で追加する
- CopyObject も無い（trash 方式を採る場合に必要になるが、後述の通り採らない）

### 孤児 manifest

push は manifest を書いてから ref を更新する。ref 更新前に失敗した push は、
どの ref からも到達できない manifest を残し、その chunk を参照し続ける。
gc は「ref から到達できない manifest」も削除対象に含めること（後述の猶予期間を適用）。

## gc の設計要件（B3 への入力）

### コマンド仕様

```
varve gc [--keep <n>] [--delete] [--remote <r>]
```

- **既定は dry-run。** 削除は `--delete` を明示したときだけ実行する（plan の決定通り）
- `--keep <n>`: 名前ごとに latest から親をたどって直近 n 世代を保持する。
  **既定値は 10**（上の実測で keep=5 と keep=3 の回収差は 7 ポイント程度であり、
  誤削除リスクに対して保持を多めに倒す）
- 出力: 名前ごとの削除対象世代、回収見込みバイト数、chunk 数

### アルゴリズム

1. 全 `refs/` を読み、名前ごとに parent 連鎖から保持世代を確定する
2. 保持対象外の manifest（到達不能な孤児を含む）を削除候補にする
3. **全名前の保持 manifest** から参照 chunk 集合を作る（プロジェクト間の
   重複排除があるため、必ず store 全体で mark する）
4. `chunks/` を列挙し、未参照かつ**猶予期間より古い**ものを sweep する
5. manifest の削除 → chunk の削除の順で実行する（逆だと途中失敗で
   「manifest はあるが chunk が無い」状態を作る）

### 並行 push との競合と対策

競合の窓: push は chunk の存在を HEAD で確認して既存ならアップロードを省略する
（`app.go` の `remote.Exists`）。HEAD の直後に gc がその chunk を削除すると、
新しい snapshot が消えた chunk を参照する。親世代からの継承（HEAD 無し）も、
親が保持対象なら chunk は消えないため安全だが、**別名プロジェクトの chunk を
HEAD 経由で共有した場合**が危ない。

チームスケールの運用に見合う多層の緩和策を採る:

1. **アドバイザリロック**: gc 開始時に `gc.lock`（開始時刻入り、prefix 直下）を置き、
   終了時に消す。push は開始時に lock があれば既定で拒否する（`--force` で無視可）。
   逆方向（push 中の gc）は検出できないが、窓を大きく狭める。
   ※ 当初案の `refs/gc.lock` は `list` が ref として解釈してしまうため
   prefix 直下に変更した（B3 実装時の判断）
2. **猶予期間**: LastModified が 24 時間以内の chunk は未参照でも消さない。
   進行中 push が「今書いたばかりの chunk」を失うことを防ぐ
3. **再マーク**: sweep 直前に snapshots/ と refs/ を再列挙し、mark 開始後に
   増えた manifest の参照を除外集合に加える
4. **検知と回復**: pull は全 chunk を SHA-256 検証するので、万一の欠損は
   静かに壊れず必ずエラーになる。回復手段は元データからの再 push
   （chunk は content-addressed なので再 push だけで穴が埋まる）

trash 方式（削除の代わりに退避して後日消す）は CopyObject の追加と
運用の複雑化に見合わないため採らない。

### 権限分離（D2 との接続)

gc は DeleteObject 権限を要求する唯一のコマンドになる。D2 のポリシー設計では
「pull のみ」「push まで」「gc まで（管理者）」の 3 段に分け、日常の資格情報では
DeleteObject を持たせないこと。

## B3 で実装するもの（まとめ)

- `store.Client.Delete`（DeleteObject）
- `gc` サブコマンド（dry-run 既定、`--keep` 既定 10、lock + 猶予 + 再マーク）
- push 側の lock 確認
- 上記のユニットテスト（memoryStore に Delete を足して gc の mark/sweep を検証）
- 検討事項（別 PR 可）: manifest の gzip 化 — **B7 で実装済み**
