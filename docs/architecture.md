# geminus のアーキテクチャ

この文書は、実装済みのコードが実際にどう動くかを書く。元の計画は `plans/geminus/plan.md` にあり、
計画と実装が食い違う点は「[計画とのずれ](#計画とのずれ)」にまとめた。食い違う場合は実装が正しい。

## 目的

geminus は、Claude Code と Hermes Agent の入出力をそのまま中継する CLI クライアント。守る性質は4つ。

1. **透過**: エージェントの出力を解釈しない。バイト列をそのまま流す。
2. **継続**: セッションを閉じても、同じ geminus セッション ID で会話を再開できる。
3. **軽量**: デーモン、DB、HTTP、常駐プロセスを持たない。状態はローカルファイルだけ。
4. **可搬**: macOS / Linux / Windows で同じコマンド体系。PTY の差は内部で吸収する。

geminus はプロトコルを実装せず、通信もしない。同じホストにあるエージェントの CLI を包むだけ。

## レイヤ構成

```
            ┌───────────────────────────────────────────┐
  terminal  │  geminus（単一バイナリ）                    │
  ──stdin──▶│                                           │
            │  internal/cli      コマンド解釈、終了コード  │
            │      │                                    │
            │      ├─▶ internal/agent    argv の組み立て  │
            │      ├─▶ internal/session  ファイルへの保存  │
            │      └─▶ internal/proxy    PTY 中継        │
            │                        │                  │
            │                        ▼                  │
            │            go-pty（Unix PTY / ConPTY）     │
            └────────────────────────┼──────────────────┘
                                     ▼
                          claude / hermes / 任意のコマンド
```

| パッケージ | 責務 | 依存 |
|---|---|---|
| `cmd/geminus` | `main` だけ。`cli.Run` の戻り値で終了する | `internal/cli` |
| `internal/cli` | サブコマンド、フラグ、`config.json`、終了コード、セッションの開始と終了の手順 | `agent`、`session`、`proxy`、`x/term` |
| `internal/agent` | バックエンドごとの argv の組み立て、検出、起動引数の秘密を伏せる処理 | 標準ライブラリ |
| `internal/session` | `session.json`、`transcript.jsonl`、`raw.log` の読み書き | 標準ライブラリ |
| `internal/proxy` | PTY の確保、raw mode、リサイズ、シグナル転送、双方向コピー、終了コード | `go-pty`、`x/term`、`x/sys` |
| `internal/version` | バージョン文字列（ビルド時に `-ldflags -X` で上書き） | 標準ライブラリ |

`agent`、`session`、`proxy` は互いに依存しない。3つを結ぶのは `cli` だけ。

外部モジュールは `github.com/aymanbagabas/go-pty`、`golang.org/x/term`、`golang.org/x/sys` の3つ。
`CGO_ENABLED=0` でクロスコンパイルできる。

### PTY を使う理由

`claude` と `hermes chat` は TUI で、パイプにつなぐと色、全画面の更新、キー入力が壊れる。
geminus は子プロセスに本物の TTY を渡し、自分はその master 側と利用者の端末の間を中継する。
エージェントからは普通の端末に見え、利用者からは普通のエージェントに見える。

`ask` だけは PTY を使わない。1往復で終わる非対話の実行なので、パイプで起動して標準出力を受け取る。

## セッションモデル

### 保存レイアウト

```
$GEMINUS_HOME（既定は ~/.geminus）
  config.json                      任意。{"agent": "hermes"} で既定のバックエンドを変える
  sessions/
    <id>/
      session.json                 メタ情報
      transcript.jsonl             追記専用のイベント列
      raw.log                      PTY の出力そのもの（--no-record では作らない）
```

- 状態ディレクトリの優先順は `--home`、環境変数 `GEMINUS_HOME`、`~/.geminus`。
- `<id>` は `YYYYMMDD-HHMMSS-<4桁の16進>`。ローカル時刻から作るので、名前順が時系列になる。
  ID はパスの一部になるため、ファイルを触る前に必ず正規表現で形を確かめる。
- ディレクトリは `0700`、ファイルは `0600`。umask で削られた場合や、既存ファイルの権限が緩んでいた場合も、作成時と追記で開くときに揃え直す。
- ディレクトリは最初の `Create` まで作らない。`ls` や `doctor` は何も書かない。

### session.json

```json
{
  "version": 1,
  "id": "20261119-122625-df5d",
  "title": "first prompt",
  "agent": "claude",
  "agent_session": { "kind": "uuid", "value": "65f7d3f5-8641-41bf-a82d-3abb247e72bd" },
  "cwd": "/Users/vvx/projekt/go/geminus",
  "created_at": "2026-11-19T12:26:25.883775+09:00",
  "updated_at": "2026-11-19T12:26:26.134521+09:00",
  "status": "exited",
  "exit_code": 0,
  "turns": 0,
  "raw_bytes": 398
}
```

- 更新は、同じディレクトリの一時ファイルに書いて `fsync` し、`rename` で置き換える。読む側は古い内容か新しい内容のどちらかだけを見る。
- `status` は `created` → `running` → `exited`。`running` の間は `pid` に中継している geminus の PID が入る。
- `agent_session` はエージェント側の会話を指すキー。claude は `kind: "uuid"`、hermes は `kind: "name"`、cmd は持たない。
- `exit_code` は子が終了するまで無い。0 も値として保存する。
- `turns` は `ask` の往復数。`run` では増えない（geminus は対話の中身を数えない）。
- `version` が現在の形式（1）より大きいファイルは読まずにエラーにする。

### transcript.jsonl とイベント

追記専用で、1行が1イベント。`seq` はセッション内の通し番号で、再開しても続きから振る。

| `type` | 内容 | 主なフィールド |
|---|---|---|
| `session.start` | エージェントを起動した | `agent`、`argv`（秘密は伏せる） |
| `input` | 利用者の入力。`ask` のプロンプトと、`run` のパイプ入力だけ | `text` |
| `output` | `raw.log` の範囲 | `offset`、`bytes` |
| `note` | geminus 自身の注記（`--fresh`、resume 非対応、記録の上限など） | `text` |
| `session.exit` | 子が終了した | `code` |

```json
{"seq":1,"ts":"…","type":"session.start","agent":"claude","argv":["claude","--session-id","65f7…","first prompt"]}
{"seq":2,"ts":"…","type":"input","text":"hello\nquit\n"}
{"seq":3,"ts":"…","type":"output","offset":0,"bytes":149}
{"seq":4,"ts":"…","type":"session.exit","code":0}
```

### イベントと offset

`output` イベントは出力の中身を持たない。`raw.log` の `[offset, offset+bytes)` を指すだけ。

- TUI は毎秒何十回も書くので、読み出しごとにイベントを作らない。連続した出力は1秒の窓でまとめ、1つの `output` にする。窓の途中で他のイベントを書くときは、溜めた分を先に書き出して順序を保つ。
- 再開したときは、既存の `raw.log` の長さを最初の offset にする。1つのセッションの `raw.log` は、全部の起動の出力がつながったものになる。
- 対話端末でのキー入力は記録しない。raw mode の入力はエスケープシーケンスの断片で、PTY のエコーとして `raw.log` 側に現れる。
- 1行は1回の `write` で書く（`O_APPEND`）。行の途中でクラッシュして改行の無い行が残っていたら、次の追記は改行で区切ってから書く。読む側は壊れた行を飛ばし、残りを返す。
- `raw.log` は 64 MiB で記録を止め、`note` を1つ残す。中継は止めない。

### 継続

geminus は会話の履歴を持たない。再開は各エージェントの resume 機能に任せる。

| 場面 | 動作 |
|---|---|
| 新規 | アダプタが新しいキーを作り、`session.json` に保存してから起動する |
| `run --resume ID` | 保存済みのキーで再開する |
| `run --resume ID --fresh` | キーだけ作り直して新しく始める。geminus のセッション（記録、タイトル）は同じ |
| resume できないアダプタ（cmd） | `note` を残して新しく起動する |
| `ask --session ID` / `ask --continue` | 保存済みのキーでワンショットを再開する |

再開するセッションは、作ったときの `cwd` で子を動かす。claude は会話をディレクトリごとに持つため。
今いる場所と違う場合は stderr に知らせる。`cwd` が相対パスだったり、もう無かったりすればエラーにする。

### 二重起動の判定

`session.json` の `status` が `running` で、`pid` のプロセスが生きていれば、そのセッションは開かれていると見なして起動を断る（`--force` で開ける）。ロックファイルは持たない。

- 読んでから書くまでの間に2つのプロセスが重なると、どちらも起動する。
- PID が別のプロセスに再利用されると「開かれている」と誤判定する。`ls` はこの区別のために、`running` で PID が生きていないものを `stale` と表示する。

### 読み取りの安全策

セッションのファイルは geminus 以外も書ける。読んだ内容をそのまま信用しない。

- `session.json`、`transcript.jsonl`、`raw.log` が通常ファイルでない場合（シンボリックリンク、名前付きパイプ、デバイス、ディレクトリ）は、読まずに `ErrNotRegular` を返す。シンボリックリンクを辿ると `geminus log` がセッションの外のファイルを表示してしまい、名前付きパイプは開くだけで書き手を待って止まる。
- 判定は `Lstat` で行う。Unix では `O_NOFOLLOW` と `O_NONBLOCK` を付けて開き、開いた後のファイルも `fstat` で確かめる（`Lstat` と `open` の間のすり替えに備える）。Windows は `Lstat` と、開いた後の確認だけ。
- 追記で開くときも同じ判定をする。Unix では `O_NOFOLLOW` と `O_NONBLOCK` を付け、開いたものが通常ファイルだと確かめてから権限を揃える。ハードリンクは区別しない（張るにはセッションのディレクトリに書ける必要があり、その時点で中身を直接書き換えられる）。
- セッションのディレクトリ `sessions/<id>` 自体がシンボリックリンクの場合も、その先を読み書きしない。`rm` はリンクだけを消す。`GEMINUS_HOME` や、そこまでの途中のディレクトリがリンクなのは構わない。
- `ls` と `show` は、`title` や `cwd` に入った制御文字を `?` に置き換えてから端末に出す。`log` は既定で制御シーケンスを除く。
- 再開時の `cwd` は絶対パスで、ディレクトリとして存在するものだけを受ける。

## アダプタ契約

バックエンドは `agent.Adapter` インターフェースで表す。アダプタは argv を組み立てるだけで、プロセスは起動しない。

```go
type Adapter interface {
    Name() string                 // --agent に渡す名前
    Binary() string               // PATH から探す実行ファイル名。cmd は空
    Capabilities() Capabilities   // Resume / Oneshot / SessionKey
    NewKey(geminusID string, fresh bool) (SessionKey, error)
    Interactive(req Request) ([]string, error)            // run 用の argv
    Oneshot(req Request, prompt string) ([]string, error) // ask 用の argv
}
```

| | claude | hermes | cmd |
|---|---|---|---|
| キー | UUIDv4（geminus が作る） | セッション名（geminus の ID） | なし |
| 新規 | `claude --session-id <uuid> [prompt]` | `hermes chat -c <name> --create-if-missing [-q prompt]` | `--` の後ろの argv そのまま |
| 再開 | `claude --resume <uuid> [prompt]` | 新規と同じ | `ErrNoResume`（cli は新規として起動する） |
| ワンショット | `claude -p --session-id\|--resume <uuid> <prompt>` | 新規の argv に `-q <prompt> --oneshot` | `ErrNoOneshot` |
| `--fresh` のキー | 新しい UUID | `<ID>-<4桁の16進>` | 適用できない（エラー） |

決めごと:

- 位置引数は空白でつないで1つのプロンプトにする。プロンプトが `-` で始まる場合は、エージェントがフラグとして読むので使い方のエラーにする。
- キーの形はアダプタが検証する（UUID の形、hermes の名前に使える文字）。`session.json` を書き換えられても、検証を通らないキーは argv に入らない。
- 実行ファイルは起動前に `PATH` で探す。無ければセッションを作る前に止め、`doctor` を案内する。
- 起動に失敗したら、今回作ったセッションは消し、既存のセッションはメタを起動前に戻す。

### 起動引数の秘密を伏せる

`session.start` の `argv` と、cmd バックエンドの既定タイトルは `agent.RedactArgv` を通す。値だけを `[REDACTED]` に置き換え、キー名は残す。

- フラグと env 形式: `--api-key VALUE`、`--token=VALUE`、`OPENAI_API_KEY=VALUE`。キー名は末尾一致で見るので、`--max-tokens` や `--tokenizer` は対象にならない。
- 1つの引数に埋まった代入: `NAME_TOKEN=value`、`NAME_TOKEN="value"`、`NAME_TOKEN='value'`。引用符は残す。
- ヘッダ: `X-Api-Key: value`、`client_secret: value`。区切りを含む複合語の名前だけを取る。`Unexpected token: }` のような文章を巻き込まないよう、`token:` や `password:` 単独は対象にしない。
- JSON のキー: `"token": "value"`、`'password': 'value'`。キーも値も引用符付きなら、単独の語でも伏せる。
- `Authorization: Bearer value`（方式名は残す）、`Authorization: value`、`scheme://user:pass@host`。引用符なしの `Authorization:` の値は、1語か2語で終わっている場合だけ伏せる（`authorization: handled by the middleware` のような文章を残すため）。

任意の文字列から秘密を見分ける方法は無いので、取りこぼしはある（引数の中の `--token value`、文章の中の `password: value` など）。`--no-record` では、コマンド名以外の引数をまとめて伏せる。
この処理の対象は argv だけで、`raw.log` の中身には手を加えない。

## PTY 中継と raw mode

`proxy.Run` が1回の中継を受け持つ。端末（`Terminal`）と子の起動（`Spawner`）はインターフェースで、テストではメモリ上の実装に差し替える。

処理の順序:

1. シグナルを受け始める（Unix は SIGINT / SIGTERM / SIGHUP / SIGQUIT / SIGPIPE）。
2. 入力が TTY なら raw mode に入る。復元は `defer` で行う。
3. 端末のサイズを読み、その大きさで PTY を確保して子を起動する。サイズが取れなければ 80x24。
4. 出力（PTY → 端末、同時に `raw.log`）と入力（端末 → PTY）を別々のゴルーチンでコピーする。
5. リサイズとシグナルを子へ伝える。
6. 子の終了を待ち、残った出力を流し切ってから PTY を閉じる。
7. 端末を復元し、シグナルの受け取りをやめる。

要点:

- **シグナルは raw mode より先に受け、復元の後で手放す。** 受けていない間に SIGTERM が届くと、既定動作で終了して端末が raw のまま残る。受けたシグナルは子のプロセスグループへ転送し、geminus 自身は子の終了を待ってから普通に終わる。SIGPIPE は転送しない。受けておくことで、閉じた標準出力への書き込みが EPIPE になり、端末を復元してから終われる。
- **raw mode 中の Ctrl-C はシグナルにならない。** バイト `0x03` として PTY を通り、子の側の端末ドライバが子に SIGINT を送る。
- **入力が TTY でない場合は raw mode に入らない。** 入力が EOF になったら EOT（`0x04`）を1回だけ子に送る。行の途中の EOT は EOF にならないので、入力が改行で終わっていなければ先に改行を1つ補う。子には PTY を渡しているので、入力は PTY がエコーする。
- **PTY を確保できなければ止まる。** パイプへ黙って切り替えると TUI が壊れるので、エラーにする。
- **記録の失敗で中継を止めない。** `raw.log` への書き込みが失敗したら記録だけをやめ、終了後に警告を出す。
- **終了コード**は子のものをそのまま返す。Unix でシグナルにより終了した場合は 128 + シグナル番号。
- **子の終了後の出力**は、PTY の EOF まで読む。孫プロセスが PTY を握っている場合や ConPTY では EOF が来ないので、出力が 100 ms 途絶えるか 2 秒たった時点で打ち切る。

OS ごとの差:

| | Unix | Windows |
|---|---|---|
| PTY | `openpty`（go-pty 経由） | ConPTY（go-pty 経由） |
| リサイズの検知 | SIGWINCH | 250 ms ごとにサイズを見比べる |
| シグナルの転送 | 子のプロセスグループへ `kill` | Ctrl-C は `0x03` を入力として送る。他は強制終了 |
| 端末の設定 | raw mode | raw mode に加えて VT 入出力を有効にする |
| echo の確認（`doctor`） | termios の `ECHO` を読む | 非対応 |

### doctor の端末チェック

`geminus doctor` は、標準入力が TTY か、端末のサイズ、echo が切れていないかを報告する。

raw mode は echo を切る。geminus が中継していないのに echo が切れているなら、端末を raw mode にしたプロセスが復元せずに終わった兆候になる。`SIGKILL` は受けられないので、geminus 自身でも起こり得る。
`doctor` は echo が切れていれば `warn` を出し、`stty sane` を1行で案内する。端末の設定は変えない。

`--json` には `terminal` オブジェクトが入る。

```json
"terminal": { "tty": true, "cols": 120, "rows": 40, "echo": true, "echo_supported": true }
```

- `tty` は標準入力が端末かどうか。
- `cols` と `rows` は、取れなければ 0。
- `echo_supported` は echo を読めたかどうか。Windows と、標準入力・標準出力・標準エラーのどれも端末でない場合は `false`。そのとき `echo` は意味を持たない。

## 失敗モード

| 状況 | 動作 |
|---|---|
| エージェントの実行ファイルが無い | セッションを作る前に止める。`doctor` を案内し、終了コード 1 |
| 未知の `--agent` | 使い方のエラー（終了コード 2） |
| PTY を確保できない | エラーで止まる。パイプへは切り替えない。今回作ったセッションは消す |
| 子を起動できない | 今回作ったセッションは消す。既存のセッションはメタを戻し、`note` を残す |
| resume 先がエージェント側に無い | geminus は検出しない。エージェントが出すエラーがそのまま見え、その終了コードで終わる。`--fresh` で抜けられる |
| `raw.log` や `transcript.jsonl` に書けない | 中継は続け、終了後に stderr へ警告を出す |
| `raw.log` が 64 MiB に達した | 記録だけを止め、`note` を残す |
| 同じセッションが開かれている | 起動を断る（`--force` で開ける） |
| `session.json` が壊れている | `ErrCorrupt`。`ls` は残りを表示して stderr に知らせ、`rm` は消せる |
| `transcript.jsonl` に壊れた行がある | その行を飛ばす。`log --json` は読めた分を出し、終了コード 1 |
| セッションのファイルが通常ファイルでない | 読まずにエラー（`ErrNotRegular`） |
| geminus が SIGTERM / SIGHUP を受けた | 子へ転送し、子の終了を待って端末を復元する |
| geminus が SIGKILL された | 端末は raw のまま残る。`doctor` が検出し、`stty sane` で直す |
| 秘密の混入 | ファイル `0600`、ディレクトリ `0700`、起動引数の値を伏せる、`--no-record` |

geminus は認証情報を読まず、保存せず、転送もしない。エージェントの環境変数をそのまま継承するだけ。

## 非目標

次は v0.1 では作らない。

- **attach**: 別の端末から動いているセッションにつなぐこと（tmux のような多重化）。続きは `--resume` で開き直す。
- **デーモン / バックエンド / リレー**: 常駐プロセスも IPC も持たない。
- **履歴の再注入**: 会話を要約してエージェントに入れ直すこと。geminus は履歴を持たず、再開はエージェントの resume に任せる。
- エージェント間の通信、複数エージェントへの同時送信、プッシュ通知。
- 認証情報の管理。
- GUI / TUI のダッシュボード。

## 計画とのずれ

`plans/geminus/plan.md` と実装が食い違う点。実装が正しい。

| 計画 | 実装 |
|---|---|
| 外部依存は `go-pty` と `x/term` の2つ | `golang.org/x/sys` を直接依存に加えた（`doctor` が termios の `ECHO` を読むため）。`x/term` が既に使っているモジュールで、`go.sum` は変わっていない |
| `run` のセッション解決は「`--resume` / 新規 / 直近」 | `run` は `--resume` か新規だけ。直近のセッションを選ぶのは `ask --continue` |
| resume 先が無ければ「エラー + `--fresh` の案内」 | geminus は検出しない。エージェント自身のエラーが出る。`--fresh` の案内は README にある |
| 非 TTY の EOF で `\x04` を1回送る | 入力が改行で終わっていなければ `\n\x04` を送る（行の途中の EOT は EOF にならない） |
| `output` イベントは読み出しごと | 1秒の窓でまとめて1イベントにする |
| `session.json` の例に `pid` が無い | `running` の間は `pid` を持つ（二重起動の判定に使う） |
| CLI の一覧に `run --force` が無い | `run --force` と `rm --force` がある |
| `ls --all` の説明が無い | `ls` は既定で新しい順に 20 件。`--all` で全件 |
| 伏せるのは `--api-key` / `--token` / `--password` 系の値 | 加えて env 形式、引数に埋まった代入（引用符付きを含む）、ヘッダ、JSON のキー、URL の userinfo も伏せる |
| シンボリックリンクについての記述が無い | セッションのファイルとディレクトリは、通常ファイル・実ディレクトリでなければ読み書きしない |
| `doctor` で端末の復元状態を確認できるようにする | echo の状態を読み、切れていれば警告する。Windows は非対応 |
| Windows は ConPTY のコンパイル保証まで | 同じ。実機での動作は未確認 |
