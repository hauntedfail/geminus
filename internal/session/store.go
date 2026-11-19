package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

// EnvHome は状態ディレクトリを上書きする環境変数。
const EnvHome = "GEMINUS_HOME"

const (
	dirPerm  fs.FileMode = 0o700
	filePerm fs.FileMode = 0o600

	sessionsDirName = "sessions"
	metaFileName    = "session.json"
	transcriptName  = "transcript.jsonl"
	rawFileName     = "raw.log"

	// 同じ秒に作られた ID が衝突したときの再試行回数。
	maxIDAttempts = 16
)

// ResolveHome は状態ディレクトリを決める。
// 優先順は override（--home）、環境変数 GEMINUS_HOME、~/.geminus。
func ResolveHome(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	if env := os.Getenv(EnvHome); env != "" {
		return filepath.Abs(env)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(userHome, ".geminus"), nil
}

// Store は1つの GEMINUS_HOME 配下のセッションを読み書きする。
type Store struct {
	home string
	now  func() time.Time
}

// Open は home を状態ディレクトリとする Store を返す。
// ディレクトリは最初の Create まで作らない（ls や doctor で副作用を出さないため）。
func Open(home string) (*Store, error) {
	if home == "" {
		return nil, errors.New("session: home directory is empty")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return nil, fmt.Errorf("session: resolve home %q: %w", home, err)
	}
	return &Store{home: abs, now: time.Now}, nil
}

// Home は状態ディレクトリの絶対パスを返す。
func (s *Store) Home() string { return s.home }

func (s *Store) sessionsDir() string { return filepath.Join(s.home, sessionsDirName) }

// Paths はセッションが使うファイルのパス。
type Paths struct {
	Dir        string
	Meta       string
	Transcript string
	Raw        string
}

// Paths は id のセッションが使うパスを返す。ファイルの有無は確かめない。
func (s *Store) Paths(id string) (Paths, error) {
	if err := checkID(id); err != nil {
		return Paths{}, err
	}
	dir := filepath.Join(s.sessionsDir(), id)
	return Paths{
		Dir:        dir,
		Meta:       filepath.Join(dir, metaFileName),
		Transcript: filepath.Join(dir, transcriptName),
		Raw:        filepath.Join(dir, rawFileName),
	}, nil
}

// CreateParams は新しいセッションの初期値。
type CreateParams struct {
	Title string
	Agent string
	// AgentSession は分かっていれば渡す。ID から作るキー（hermes）は Create の後で Update する。
	AgentSession *AgentSession
	Cwd          string
}

// Create は新しいセッションのディレクトリと session.json を作る。
func (s *Store) Create(p CreateParams) (Meta, error) {
	if err := s.ensureSessionsDir(); err != nil {
		return Meta{}, err
	}
	now := s.timestamp()
	id, err := s.makeSessionDir(now)
	if err != nil {
		return Meta{}, err
	}

	meta := Meta{
		Version:      SchemaVersion,
		ID:           id,
		Title:        p.Title,
		Agent:        p.Agent,
		AgentSession: p.AgentSession,
		Cwd:          p.Cwd,
		CreatedAt:    now,
		UpdatedAt:    now,
		Status:       StatusCreated,
	}.clone()
	if err := s.writeMeta(meta); err != nil {
		// 中身の無いセッションを残さない。
		if rmErr := os.RemoveAll(filepath.Join(s.sessionsDir(), id)); rmErr != nil {
			return Meta{}, errors.Join(err, rmErr)
		}
		return Meta{}, err
	}
	return meta, nil
}

// Load は id の session.json を読む。
// セッションが無ければ ErrNotFound、読めない内容なら ErrCorrupt、
// session.json が通常ファイルでなければ（シンボリックリンクなど）ErrNotRegular を返す。
func (s *Store) Load(id string) (Meta, error) {
	paths, err := s.existingPaths(id)
	if err != nil {
		return Meta{}, err
	}
	data, err := readRegular(paths.Meta)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Meta{}, fmt.Errorf("session %s: %w: %s is missing", id, ErrCorrupt, metaFileName)
	case err != nil:
		return Meta{}, fmt.Errorf("session %s: read %s: %w", id, metaFileName, err)
	}

	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return Meta{}, fmt.Errorf("session %s: %w: %v", id, ErrCorrupt, err)
	}
	switch {
	case meta.Version > SchemaVersion:
		return Meta{}, fmt.Errorf("session %s: %w: version %d (this build reads up to %d)", id, ErrUnsupportedVersion, meta.Version, SchemaVersion)
	case meta.Version < 1:
		return Meta{}, fmt.Errorf("session %s: %w: missing version", id, ErrCorrupt)
	case meta.ID != id:
		return Meta{}, fmt.Errorf("session %s: %w: %s holds id %q", id, ErrCorrupt, metaFileName, meta.ID)
	}
	return meta, nil
}

// Save は meta を session.json に原子的に書き、updated_at を進めた値を返す。
// 渡した meta は変更しない。削除済みのセッションは作り直さず ErrNotFound を返す。
func (s *Store) Save(meta Meta) (Meta, error) {
	if _, err := s.existingPaths(meta.ID); err != nil {
		return Meta{}, err
	}

	next := meta.clone()
	next.Version = SchemaVersion
	next.UpdatedAt = s.timestamp()
	if err := s.writeMeta(next); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 確認と書き込みの間に別プロセスが消した。
			return Meta{}, fmt.Errorf("session %s: %w", meta.ID, ErrNotFound)
		}
		return Meta{}, err
	}
	return next, nil
}

// Update は id のメタを読み、change の返す値を保存する。
// change が ID を書き換えても元の ID で保存する。
func (s *Store) Update(id string, change func(Meta) Meta) (Meta, error) {
	current, err := s.Load(id)
	if err != nil {
		return Meta{}, err
	}
	next := change(current.clone())
	next.ID = id
	return s.Save(next)
}

// List は全セッションを updated_at の降順（同時刻は ID の降順）で返す。
//
// 読めないセッションがあっても残りは返し、読めなかった分を error にまとめる。
// 呼び出し側は error が非 nil でも返ってきたスライスを使ってよい。
func (s *Store) List() ([]Meta, error) {
	entries, err := os.ReadDir(s.sessionsDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	var metas []Meta
	var broken []error
	for _, entry := range entries {
		if !entry.IsDir() || !ValidID(entry.Name()) {
			continue
		}
		meta, err := s.Load(entry.Name())
		if err != nil {
			broken = append(broken, err)
			continue
		}
		metas = append(metas, meta)
	}
	sort.SliceStable(metas, func(i, j int) bool {
		if !metas[i].UpdatedAt.Equal(metas[j].UpdatedAt) {
			return metas[i].UpdatedAt.After(metas[j].UpdatedAt)
		}
		return metas[i].ID > metas[j].ID
	})
	return metas, errors.Join(broken...)
}

// Delete は id のセッションをディレクトリごと消す。無ければ ErrNotFound を返す。
// session.json が壊れていても消せる。
func (s *Store) Delete(id string) error {
	paths, err := s.Paths(id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(paths.Dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("session %s: %w", id, ErrNotFound)
		}
		return fmt.Errorf("session %s: %w", id, err)
	}
	if err := os.RemoveAll(paths.Dir); err != nil {
		return fmt.Errorf("session %s: delete: %w", id, err)
	}
	return nil
}

// timestamp は現在時刻から単調時計の読みを落とす。JSON を往復した値と一致させるため。
func (s *Store) timestamp() time.Time {
	return s.now().Round(0)
}

func (s *Store) ensureSessionsDir() error {
	if err := os.MkdirAll(filepath.Dir(s.home), dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(s.home), err)
	}
	for _, dir := range []string{s.home, s.sessionsDir()} {
		if err := ensurePrivateDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// ensurePrivateDir は dir が無ければ 0700 で作る。
// 既にあるディレクトリの権限は変えない（利用者が --home で渡した場所かもしれない）。
func ensurePrivateDir(dir string) error {
	err := os.Mkdir(dir, dirPerm)
	if err == nil {
		// umask で削られた場合も 0700 に揃える。
		if err := os.Chmod(dir, dirPerm); err != nil {
			return fmt.Errorf("chmod %s: %w", dir, err)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	info, statErr := os.Stat(dir)
	if statErr != nil {
		return fmt.Errorf("stat %s: %w", dir, statErr)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", dir)
	}
	return nil
}

// makeSessionDir は未使用の ID を選び、そのディレクトリを作る。
// os.Mkdir が既存を拒否するので、ID の予約とディレクトリ作成が1操作で済む。
func (s *Store) makeSessionDir(now time.Time) (string, error) {
	for attempt := 0; attempt < maxIDAttempts; attempt++ {
		id, err := NewID(now)
		if err != nil {
			return "", err
		}
		dir := filepath.Join(s.sessionsDir(), id)
		err = os.Mkdir(dir, dirPerm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create session directory: %w", err)
		}
		if err := os.Chmod(dir, dirPerm); err != nil {
			return "", fmt.Errorf("chmod %s: %w", dir, err)
		}
		return id, nil
	}
	return "", fmt.Errorf("create session directory: no free id after %d attempts", maxIDAttempts)
}

func (s *Store) writeMeta(meta Meta) error {
	paths, err := s.Paths(meta.ID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("session %s: encode: %w", meta.ID, err)
	}
	if err := writeFileAtomic(paths.Meta, append(data, '\n')); err != nil {
		return fmt.Errorf("session %s: write %s: %w", meta.ID, metaFileName, err)
	}
	return nil
}

// writeFileAtomic は同じディレクトリの一時ファイルに書いてから rename で置き換える。
// 読む側は古い内容か新しい内容のどちらかだけを見る。
func writeFileAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			err = errors.Join(err, tmp.Close(), removeIfExists(tmpPath))
		}
	}()

	if err = tmp.Chmod(filePerm); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return replaceFile(tmpPath, path)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// replaceFile は rename で置き換える。Windows では、別プロセスが読んでいる間の
// rename が共有違反で失敗するので、短く待って再試行する。
func replaceFile(from, to string) error {
	attempts := 1
	if runtime.GOOS == "windows" {
		attempts = 5
	}
	var err error
	for i := 0; i < attempts; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	return err
}
