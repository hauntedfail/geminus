# geminus — 軽量エージェントクライアント 実装計画

- 状態: **実装完了・検証済み（2026-11-19）**
- 対象リポジトリ: `/Users/vvx/projekt/go/geminus`
- 言語: Go（単一静的バイナリ、CGO無効）
- 作成: 2026-11-19

---

## 1. 目的

`geminus` は、**Claude Code と Hermes Agent の入出力をそのまま中継するだけの、薄い CLI クライアント**である。

守るべき性質は次の4つ。

1. **透過**: エージェントの出力を解釈・再構成しない。バイトをそのまま流す。
2. **継続**: セッションを閉じても、同じ geminus セッション ID で会話と作業を再開できる。
3. **軽量**: デーモンなし、DBなし、HTTPなし、常駐プロセスなし。状態はローカルファイルのみ。
4. **可搬**: macOS / Linux / Windows で同一コマンド体系。PTY 差異は内部で吸収する。

`geminus` は geminise エコシステムの**クライアント側**に位置するが、`geminish`（Hermes 専用クライアント）とは別物である。
`geminus` はプロトコルを実装しない。通信もしない。単一ホスト上のエージェント CLI を包むだけ。

### 非目標（v0.1 では作らない）

- デーモン / バックエンド / リレー（それは `geminised` の領域）
- 別ターミナルからのライブ attach（tmux 的な多重化）。継続は **resume** で満たす
- エージェント間通信、グループ fan-out、push 通知
- 会話履歴の要約・再注入（v0.2 の `--replay` 候補）
- 認証情報の管理（エージェントの既存認証をそのまま使う）
- GUI / TUI ダッシュボード

---

## 2. アーキテクチャ

```
            ┌──────────────────────────────────────────┐
  terminal  │  geminus (single binary)                 │
  ──stdin──▶│                                          │
            │  internal/cli      コマンド解釈・設定解決  │
            │      │                                   │
            │      ├─▶ internal/agent   アダプタ選択     │
            │      ├─▶ internal/session セッション永続化 │
            │      └─▶ internal/proxy   PTY 中継        │
            │                        │                 │
            │                        ▼                 │
            │            go-pty (unix pty / ConPTY)    │
            └────────────────────────┼─────────────────┘
                                     ▼
                          claude / hermes / any cmd
```

### レイヤと責務

| パッケージ | 責務 | 依存 |
|---|---|---|
| `internal/cli` | サブコマンド、フラグ、設定読込、終了コード | stdlib |
| `internal/agent` | バックエンド検出、能力表、起動引数の組み立て | stdlib |
| `internal/session` | セッションメタ・イベントログ・生ログの読み書き | stdlib |
| `internal/proxy` | PTY 確保、raw mode、リサイズ、双方向コピー、終了コード | `go-pty`, `x/term` |
| `cmd/geminus` | `main` のみ（薄い） | 上記 |

**外部依存は2つだけ**: `github.com/aymanbagabas/go-pty`（Unix PTY + Windows ConPTY を単一 API で提供）と `golang.org/x/term`（raw mode / 端末サイズ）。
それ以外は標準ライブラリで完結させる。`CGO_ENABLED=0` でクロスコンパイル可能。

### なぜ PTY か

`claude` と `hermes chat` は TUI であり、パイプでは色・全画面更新・キー入力が壊れる。
geminus は子プロセスに**本物の TTY** を与え、自分はその master 側とユーザ端末の間を中継する。
これにより「エージェントから見ると普通の端末」「ユーザから見ると普通のエージェント」という透過性が成立する。

---

## 3. セッションモデル

### 3.1 保存レイアウト

```
$GEMINUS_HOME (既定: ~/.geminus)
  config.json                      # 任意。既定エージェントなど
  sessions/
    <id>/
      session.json                 # メタ（原子的 rename で更新）
      transcript.jsonl             # 追記専用イベント列
      raw.log                      # PTY 生バイト（--no-record で無効）
```

- `<id>` は `YYYYMMDD-HHMMSS-<4hex>`。時系列でソートでき、人間が読める。
- ディレクトリ `0700`、ファイル `0600`。会話には秘密が混ざり得るため既定で厳しくする。

### 3.2 session.json

```json
{
  "version": 1,
  "id": "20261119-105700-a1b2",
  "title": "geminus design",
  "agent": "claude",
  "agent_session": { "kind": "uuid", "value": "3f2c...-..." },
  "cwd": "/Users/vvx/projekt/go/geminus",
  "created_at": "2026-11-19T10:57:00+09:00",
  "updated_at": "2026-11-19T11:02:11+09:00",
  "status": "exited",
  "exit_code": 0,
  "turns": 3,
  "raw_bytes": 18422
}
```

### 3.3 transcript.jsonl

追記専用。1行1イベント。`raw.log` への**バイトオフセット**を持ち、生ログと厳密に対応させる。

```json
{"seq":1,"ts":"...","type":"session.start","agent":"claude","argv":["claude","--session-id","..."]}
{"seq":2,"ts":"...","type":"output","offset":0,"bytes":812}
{"seq":3,"ts":"...","type":"input","text":"..."}          // ask / 非TTY のみ
{"seq":4,"ts":"...","type":"note","text":"resume unsupported"}
{"seq":5,"ts":"...","type":"session.exit","code":0}
```

`argv` は `--api-key` / `--token` / `--password` 系の**値だけを伏せる**（キー名は残す）。

### 3.4 セッション継続（最重要）

geminus は自前の会話履歴を持たない。**各エージェントのネイティブ resume に委譲する**。

| agent | 新規 | 再開 | キー |
|---|---|---|---|
| `claude` | `claude --session-id <uuid>` | `claude --resume <uuid>` | geminus が生成する UUIDv4 |
| `hermes` | `hermes chat -c <id> --create-if-missing` | 同左（名前一致で再開） | geminus の id をセッション名に使う |
| 任意コマンド | そのまま起動 | 非対応（警告し、新規として起動） | なし |

- `claude --session-id <uuid>` は UUID 必須。geminus が生成し `session.json` に保存する。
- `hermes -c <name> --create-if-missing` は「無ければ作る、あれば再開」で冪等。geminus id をそのまま名前にする。
- 再開先が消えていた場合（claude 側のクリーンアップ等）は失敗する。`--fresh` を付けると geminus セッションは維持したまま**エージェント側だけ新規**にする。行き止まりを作らないための逃げ道。

### 3.5 同時実行

同一セッションを2プロセスで同時に開かない。`session.json` の `status` が `running` かつ PID が生きている場合は起動を拒否する（`--force` で上書き）。
ロックファイルは作らない（PID 生存確認で足りる。クラッシュ時の残留ロックを避ける）。

---

## 4. CLI 仕様

```
geminus run [--agent NAME] [--title T] [--resume ID] [--fresh] [--no-record] [--] [prompt...]
geminus ask [--agent NAME] [--session ID] [--continue] [--json] "prompt"
geminus ls [--json] [--all]
geminus show <id> [--json]
geminus log <id> [--raw] [--json] [--tail N]
geminus rm <id> [--force]
geminus agents            # 検出済みバックエンドと能力
geminus doctor            # PTY / 端末 / バックエンド診断
geminus version
```

グローバル: `--home DIR`（`GEMINUS_HOME` を上書き）、`--agent NAME`（既定: config、無ければ `claude`）、`--json`、`-v`。

### 終了コード

| code | 意味 |
|---|---|
| 0 | 正常終了 |
| 1 | geminus 自体のエラー（I/O、PTY 確保失敗など） |
| 2 | 使い方のエラー |
| それ以外 | 子プロセスの終了コードをそのまま返す（`run` / `ask`） |

### `run` の挙動

1. セッション解決（`--resume` / 新規 / 直近）
2. 端末が TTY なら raw mode に入る（`defer` で必ず復元）
3. PTY を確保し、アダプタが組み立てた argv で子を起動
4. stdin → PTY、PTY → stdout を双方向コピー（出力は `raw.log` に tee）
5. `SIGWINCH` でリサイズ、`SIGINT`/`SIGTERM` は子のプロセスグループへ転送
6. 子の終了コードを保存して同じ値で終了

stdin が TTY でない場合（パイプ）は raw mode に入らず、EOF で `\x04` を1回送る。

### `ask` の挙動

`run` を非対話にしたもの。PTY は使わずパイプで子を起動し、stdout を捕捉して表示・記録する。
claude は `-p`、hermes は `chat -q ... --oneshot`。セッション継続キーは `run` と同じ。

---

## 5. 失敗モードと安全側の設計

| 状況 | 挙動 |
|---|---|
| エージェントのバイナリが無い | 起動前に検出し、`doctor` 相当の案内を出して終了コード 1 |
| PTY 確保失敗 | パイプへ黙ってフォールバックしない。明示エラーで停止（TUI 破壊を避ける） |
| resume 先が存在しない | エラー + `--fresh` の案内 |
| 記録の書き込み失敗 | セッションは続行し、stderr に警告（会話を止めない） |
| raw.log が上限超過 | 既定 64 MiB で記録停止し `note` イベントを残す（会話は継続） |
| 端末の復元漏れ | `defer` + シグナルハンドラの二重化。`doctor` で復元状態を確認可能に |
| 同一セッションの二重起動 | 拒否（`--force` で明示上書き） |
| 秘密の混入 | ファイル 0600 / ディレクトリ 0700、argv の値レダクト、`--no-record` |

**やらないこと**: 認証情報の読み取り・保存・転送。geminus はエージェントの環境をそのまま継承するだけで、中身を見ない。

---

## 6. 検証戦略

| 段階 | 方法 | 合格条件 |
|---|---|---|
| 単体 | `go test ./internal/...` | session/agent/proxy の表駆動テストが通る |
| 中継 | fake agent（`test/fakeagent`）を PTY 経由で起動 | 出力一致、終了コード伝播、リサイズ、raw.log オフセット一致 |
| 非TTY | stdin パイプで `run` | raw mode に入らず、EOF で正常終了 |
| E2E | ビルドしたバイナリをサブプロセスで実行 | `ls`/`log`/`show`/`rm` が期待通り。壊れた session.json で panic しない |
| クロスビルド | `scripts/crossbuild.sh` | darwin/linux/windows × amd64/arm64 の6ターゲットが成功（Windows は ConPTY 経路のコンパイル保証） |
| 手動 | 実 `claude` / 実 `hermes` で1往復、`--resume` で継続 | 文脈が保持される |
| 手動（OS） | Windows 実機（mirai）、Linux（sibyl） | `run` が TUI として成立する |

CI は v0.1 では置かない（owner の既存運用に合わせ、`scripts/` と README にコマンドを明記）。

---

## 7. 決定事項（2026-11-19 承認済み）

1. **透過バイト中継**。出力の構造化・要約・再注入はしない。セッション継続は各エージェントの resume に委譲する。
2. **バックエンドなし**。単一バイナリ、ローカルファイルのみ。デーモンも IPC も持たない。
3. **attach は非目標**。継続は resume で満たす。ライブ多重化は需要が出たら v0.2 で再検討。
4. **依存は go-pty と x/term の2つ**。完全ゼロ依存（PTY 自前実装）は工数と Windows リスクが跳ね上がるため採らない。
5. **状態は `~/.geminus`**（`GEMINUS_HOME` で上書き）。
6. **モジュールパスは `github.com/hauntedfail/geminus`**。`go install` 可能にしておく。

## 8. 承認結果（2026-11-19）

- **A. proxy の意味**: 透過バイト中継 + 各エージェントのネイティブ resume 委譲で確定。geminus 側の履歴再注入は v0.2 以降。
- **B. v0.1 の範囲**: `run` + `ask` + `ls/show/log/rm/agents/doctor` で確定。
- **C. 依存方針**: `go-pty` + `x/term` の2つに限定で確定。
- **D. 状態ディレクトリ**: `~/.geminus`（`GEMINUS_HOME` で上書き）で確定。

## 9. 前提・リスク

- ~~Go ツールチェーンが未導入~~ → 2026-11-19 に Go 1.27.4 を導入済み（`/opt/homebrew/bin/go`）。
- `claude --session-id` は UUID 必須。生成と保存で吸収する。
- `hermes -c <name>` のセッション名に使える文字種は実装時に実機で確認する（geminus id は英数字とハイフンのみなので安全側）。
- Windows の ConPTY は**コンパイル保証まで**を v0.1 の合格とし、実機確認は mirai で手動実施する。
- 生ログは会話そのもの。バックアップや共有の対象にしない（README に明記）。

## 10. タスク分割

| ID | 内容 | 依存 |
|---|---|---|
| gmn-001 | 足場（go.mod、CLI 骨格、version/help） | — |
| gmn-002 | セッションストア（メタ・イベント・原子的更新） | 001 |
| gmn-003 | エージェントアダプタ（claude / hermes / generic） | 001 |
| gmn-004 | PTY プロキシ（raw mode・リサイズ・終了コード） | 001 |
| gmn-005 | CLI 結線（run / ask / ls / show / log / rm / agents / doctor） | 002,003,004 |
| gmn-006 | E2E（fake agent、非TTY、破損データ耐性） | 005 |
| gmn-007 | ドキュメントとクロスビルド | 005 |

実装は**アーキテクチャ承認後**に開始する。承認まではすべて `blocked`。

---

## 11. 実装結果（2026-11-19）

7 unit すべて完了。`/Users/vvx/projekt/go/geminus` に単一バイナリの Go 実装がある。

### 検証済み（実行して確認した事実）

| 項目 | 結果 |
|---|---|
| `go build ./...` / `go vet ./...` | exit 0 |
| `go test ./... -count=1` | 579 tests PASS / FAIL 0 / SKIP 0 |
| `-race` / `-count=8` | PASS、フレークなし |
| カバレッジ | agent 96.5% / cli 93.5% / proxy 90.1% / session 87.0% / version 100% |
| クロスビルド | darwin/linux/windows × amd64/arm64 の6ターゲット成功 |
| 実 claude（ask） | `GEMINUS_OK` を返し exit 0 |
| 実 claude（resume） | 同一セッションで前回トークンを正答 |
| 実 hermes（ask / resume） | `HERMES_GEMINUS_OK` を返し、再開時も正答 |
| 実 PTY 入力 | `sh -c 'read x; echo GOT:$x'` に hello を送り `GOT:hello`、終了コード 0 |
| 端末復元 | geminus 終了後、外側 PTY の ECHO が復元されている |
| 秘密のレダクト | `API_KEY="s3cr3t"` 等が session.json / transcript に残らない（raw.log には子の出力として残る） |
| ドキュメント | README 179行、docs/architecture.md 309行、crossbuild.sh 65行 |

### 未検証（正直な残り）

- **Windows の実機実行**。クロスビルドと vet のみ。ConPTY と VT モードは未確認。Windows 実機は mirai で手動確認する。
- Linux は 2026-11-19 にコンテナで確認済み（`bash scripts/test-linux.sh`、`golang:1.27` / linux/arm64、一般利用者で実行）。`go vet` exit 0、579 tests PASS / FAIL 0 / SKIP 0。実 PTY を使うテスト（EIO での終端を含む）もこの中で通っている。Linux 上で実 claude / 実 hermes を動かす確認（sibyl）は未実施。
- ~~実 claude の対話 TUI での1往復~~ → 2026-11-19 に確認済み。PTY 越しに `geminus run --agent claude` を起動し、入力欄へのエコー、返答（プロンプトに含まれない文字列 `GEM_NUS77`）、`/exit` での終了コード 0、`ls` への記録まで通った。前回の失敗は、TUI の起動が終わる前にプロンプトを送っていたハーネス側の問題だった。対話 TUI での `--resume` は未確認（`ask` の resume は確認済み）。
- `go install ...@latest` はリポジトリ未公開のため未確認。
- 同時起動のロックは無い（設計どおり。`Meta.Active()` + `--force`）。

### 次の判断が必要なもの

1. git 管理を始めるか（`git init` + 初回コミット。署名コミットの方針に従う）。**未実行**。
2. GitHub に `hauntedfail/geminus` を作るか（公開・push は owner 承認事項）。**未実行**。
3. Windows 実機（mirai）での受け入れテスト。Linux のテスト一式はコンテナで通した。sibyl で実エージェントを動かす確認は、VPS に触るので owner の指示を待つ。
