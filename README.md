# geminus

Claude Code と Hermes Agent の入出力を、PTY 越しにそのまま中継する薄い CLI クライアント。

- エージェントの出力は解釈も再構成もしない。バイト列をそのまま端末へ流す。
- セッションを閉じても、同じ geminus セッション ID で会話を再開できる。再開は各エージェントの resume 機能に任せる。
- デーモン、DB、HTTP、常駐プロセスは無い。状態はローカルファイルだけに置く。
- 単一バイナリ（CGO 無効）。macOS / Linux / Windows で同じコマンド体系。

設計は [docs/architecture.md](docs/architecture.md) にある。

## インストール

Go 1.23 以降が必要。

```sh
go install github.com/hauntedfail/geminus/cmd/geminus@latest
```

ソースからビルドする場合は、リポジトリの直下で次を実行する。

```sh
go build -o geminus ./cmd/geminus
./geminus version
```

配布用のバイナリは `bash scripts/crossbuild.sh` で作る。`dist/` に darwin / linux / windows × amd64 / arm64 の6つができる。

エージェント本体（`claude`、`hermes`）は別に入れて、`PATH` に置く。認証もエージェント側のものをそのまま使う。geminus は認証情報を読まないし、保存もしない。

## クイックスタート

```sh
# 環境を確かめる（PTY、端末、見つかったエージェント）
geminus doctor
geminus agents

# 対話セッションを始める（既定は claude）
geminus run
geminus run "このリポジトリの構成を説明して"
geminus run --agent hermes --title "設計の相談"

# 終了すると、再開用のコマンドが stderr に出る
#   geminus: session 20261119-122625-df5d ended (exit 0). Resume with: geminus run --resume 20261119-122625-df5d
geminus run --resume 20261119-122625-df5d

# 1往復だけ聞く（PTY は使わない）
geminus ask "go.mod の依存を一覧にして"
geminus ask --continue "その中で直接依存はどれ？"      # このディレクトリで最後に更新したセッションを続ける
geminus ask --session 20261119-122625-df5d "続き"      # セッションを指定して続ける
cat notes.txt | geminus ask "要約して"                 # 標準入力はエージェントにそのまま渡る
geminus ask --json "hello"                             # {"session","agent","exit_code","output"}

# セッションを見る
geminus ls                    # 新しい順に 20 件。--all で全件、--json で JSON
geminus show 20261119-122625-df5d
geminus log 20261119-122625-df5d              # 記録した出力（制御シーケンスは除く）
geminus log 20261119-122625-df5d --tail 20    # 末尾 20 行
geminus log 20261119-122625-df5d --raw        # 制御シーケンスを残す
geminus log 20261119-122625-df5d --json       # transcript.jsonl のイベント列

# 消す
geminus rm 20261119-122625-df5d
```

任意のコマンドも PTY 越しに起動できる。`--` の後ろがそのままコマンド行になる。

```sh
geminus run --agent cmd -- sh -c 'echo hello'
```

共通フラグは `--home DIR`、`--agent NAME`、`--json`、`-v`。コマンド名の前後どちらに書いてもよい。
各コマンドの詳細は `geminus help <command>` で出る。

### 終了コード

| コード | 意味 |
|---|---|
| 0 | 正常終了 |
| 1 | geminus 自体のエラー（I/O、PTY 確保の失敗など） |
| 2 | 使い方のエラー |
| その他 | `run` / `ask` は子プロセスの終了コードをそのまま返す。Unix でシグナルにより終了した子は 128 + シグナル番号 |

## バックエンド

| `--agent` | 新規 | 再開（`run --resume`） | ワンショット（`ask`） |
|---|---|---|---|
| `claude` | `claude --session-id <uuid>` | `claude --resume <uuid>` | `claude -p --session-id <uuid> <prompt>`（続きは `--resume <uuid>`） |
| `hermes` | `hermes chat -c <name> --create-if-missing` | 新規と同じ（名前が一致すれば再開） | 左に `-q <prompt> --oneshot` を足す |
| `cmd` | `--` の後ろのコマンド行をそのまま起動 | 非対応。注記を出して新しく起動する | 非対応 |

- `<uuid>` は geminus が作り、`session.json` に保存する。
- `<name>` には geminus のセッション ID をそのまま使う。
- `--resume ID --fresh` は、geminus のセッションはそのままに、エージェント側の会話だけを新しくする。
- 既定のバックエンドは `claude`。`$GEMINUS_HOME/config.json` に `{"agent": "hermes"}` と書けば変えられる。
- 再開するセッションは、最初に作ったときのディレクトリで動かす（claude は会話をディレクトリごとに持つため）。今いる場所と違う場合は stderr に出る。

## セッションの保存場所

```
~/.geminus/                         # GEMINUS_HOME か --home で変えられる
  config.json                       # 任意
  sessions/
    20261119-122625-df5d/           # ID は YYYYMMDD-HHMMSS-<4桁の16進>
      session.json                  # メタ情報
      transcript.jsonl              # イベント列（起動、入力、出力の範囲、注記、終了）
      raw.log                       # PTY の出力そのもの
```

- ディレクトリは `0700`、ファイルは `0600` で作る。`geminus doctor` は、他のユーザが読める状態を警告する。
- 場所の優先順は `--home DIR`、環境変数 `GEMINUS_HOME`、`~/.geminus`。
- `raw.log` は 64 MiB で記録を止める。会話はそのまま続く。

### 記録される内容

**`raw.log` には会話そのものが入る。** エージェントが画面に出したものは、貼り付けたコード、読んだファイルの中身、表示されたトークンを含めてすべて残る。

- `~/.geminus` をバックアップやクラウド同期の対象に入れるときは、会話を置いてよい場所かを確かめる。
- セッションのディレクトリや `geminus log` の出力を共有する前に、中身を読む。
- 記録を残したくないときは `geminus run --no-record` を使う。`raw.log` と出力イベントを書かず、起動引数はコマンド名だけを残し、プロンプトをタイトルにもしない。

`transcript.jsonl` に残す起動引数は、秘密らしい値を `[REDACTED]` に置き換える。

- `--api-key VALUE`、`--token=VALUE`、`OPENAI_API_KEY=VALUE`
- 1つの引数の中の `NAME_TOKEN=value` / `NAME_TOKEN="value"` / `NAME_TOKEN='value'`
- `X-Api-Key: value`、`"token": "value"`、`Authorization: Bearer value`、`https://user:pass@host`

これは起動引数だけが対象で、`raw.log` の中身には手を加えない。また、任意の文字列から秘密を見分ける方法は無いので、取りこぼしはある（たとえば `sh -c 'curl --token abc'` の `abc`）。残したくない値は環境変数で渡すか、`--no-record` を使う。

`session.json`、`transcript.jsonl`、`raw.log` がシンボリックリンクや名前付きパイプなどにすり替わっている場合、geminus は辿らずにエラーにする。

## 既知の制約

- **パイプ入力は画面にエコーされる。** `printf 'hello\n' | geminus run` のように標準入力が端末でない場合も、子には PTY を渡す。PTY は受け取った入力をエコーするので、出力と `raw.log` に入力の写し（と `^D`）が混ざる。入力の終わりは EOT（Ctrl-D）として1回だけ子に送る。
- **Windows はコンパイルの確認だけ。** ConPTY の経路はビルドが通ることまでを確かめている。実機での動作は未確認。macOS と Linux はテスト一式が通っている。
- **同時起動のロックは無い。** `session.json` の `status` と PID を見て、動いているセッションの二重起動を断る（`--force` で開ける）。ロックファイルは持たないので、2つのプロセスがごく短い間に重なると、どちらも起動する。PID の再利用で「動いている」と誤判定することもある。
- **別の端末からの attach はできない。** 続きは `--resume` で開き直す。
- **会話の履歴を geminus は持たない。** 再開できるかはエージェント側の履歴次第。エージェントが履歴を消すと再開できない。
- `ask` は標準出力だけを記録する。標準エラーは端末へ出すが、記録しない。
- `cmd` バックエンドは `--resume` しても会話は続かない（同じ geminus セッションに記録が追記されるだけ）。

## トラブルシュート

**再開しようとすると、エージェントが「セッションが無い」と言って終わる。**
エージェント側の履歴が消えている。`--fresh` を付けると、geminus のセッション（記録とタイトル）は残したまま、エージェント側だけ新しく始める。

```sh
geminus run --resume 20261119-122625-df5d --fresh
```

**打った文字が表示されない。改行がずれる。**
端末が raw mode のまま残っている。geminus が強制終了（`kill -9` など）されると起きる。`geminus doctor` の `terminal echo` が `warn` になる。次で直る。

```sh
stty sane
```

`stty sane` も見えないまま打つことになる。Enter で確定できなければ Ctrl-J を押す。Windows では端末を開き直す。

**`session ... is already open in process N` と出て開けない。**
そのセッションを別の geminus が開いている。前回の geminus が落ちて PID が別のプロセスに再利用された場合も出る。`geminus ls` で `running` か `stale` かを確かめ、動いていないことが分かっていれば `--force` を付ける。

**`agent binary not found` と出る。**
`geminus doctor` でどのエージェントが `PATH` にあるかを見る。

**壊れたセッションが `ls` で警告になる。**
`geminus rm <id>` は `session.json` が読めなくても消せる。

## 開発

```sh
go build ./...
go vet ./...
go test ./... -count=1
go test ./... -count=1 -race
bash scripts/crossbuild.sh
bash scripts/test-linux.sh        # docker の Linux コンテナで vet と test
```

テストは実際の PTY と偽エージェント（`test/fakeagent`）を使う。CI は置いていない。
`scripts/test-linux.sh` は macOS / Windows の開発機から Linux の PTY 経路を確かめるためのもので、引数は `go test` にそのまま渡る。
