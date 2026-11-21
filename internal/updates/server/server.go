// Package server builds, signs and hands out geminus releases. It is the
// whole of cmd/geminus-latest-channel apart from flags, the key file and the
// listener.
//
//	GET  /geminus/updates/latest              the manifest of the newest release
//	GET  /geminus/updates/{version}           the manifest of that release
//	GET  /geminus/updates/{version}/{target}  one program
//	POST /geminus/updates/{version}           build and publish that release
//	GET  /geminus/updates/{version}/build     how that build is going
//
// Every request is signed with the group's secret (package updates). Nothing
// can be uploaded: a release is what this computer built from the tag of its
// version, signed with a key only this computer holds. It is visible once
// its manifest is written, and never changes afterwards.
package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hauntedfail/geminus/internal/updates"
)

const (
	manifestName = "manifest.json"
	latestName   = "latest"
	tmpPrefix    = ".tmp-"
	// defaultLimit bounds one build.
	defaultLimit = 15 * time.Minute
	// maxReason is how much of what a failed build said is kept: its end,
	// where a compiler says what is wrong.
	maxReason = 2000
)

// Config is what a Server works with.
type Config struct {
	// Root is the directory the releases are kept in.
	Root *os.Root
	// Secret is the group's secret; every request must be signed with it.
	Secret []byte
	// Key signs the releases built here.
	Key ed25519.PrivateKey
	// Build makes the programs of release version from its source and
	// leaves them in dir, an empty directory inside Root, one file per
	// target named "<os>-<arch>", and nothing else. It stops when ctx ends.
	Build func(ctx context.Context, version, dir string) error
	// Limit bounds one build; fifteen minutes if zero.
	Limit time.Duration
	Now   func() time.Time
	Log   *slog.Logger
}

// Server is the handler for the releases of one directory.
type Server struct {
	cfg  Config
	root *os.Root
	log  *slog.Logger
	mux  *http.ServeMux
	// ctx ends when the server is closed, and with it a build.
	ctx    context.Context
	stop   context.CancelFunc
	builds sync.WaitGroup
	// mu serializes everything that changes the directory, and guards the
	// fields below.
	mu sync.Mutex
	// building is the version being built, or "".
	building string
	// last is how the build asked for last went.
	last updates.Build
}

// New returns the server for the releases kept in c.Root.
func New(c Config) *Server {
	if c.Limit <= 0 {
		c.Limit = defaultLimit
	}
	s := &Server{cfg: c, root: c.Root, log: c.Log, mux: http.NewServeMux()}
	s.ctx, s.stop = context.WithCancel(context.Background())
	s.sweep()
	s.mux.HandleFunc("GET "+updates.Path+"/latest", s.signed(s.latest))
	s.mux.HandleFunc("GET "+updates.Path+"/{version}", s.signed(s.manifest))
	s.mux.HandleFunc("GET "+updates.Path+"/{version}/{target}", s.signed(s.program))
	s.mux.HandleFunc("POST "+updates.Path+"/{version}", s.signed(s.release))
	s.mux.HandleFunc("GET "+updates.Path+"/{version}/build", s.signed(s.state))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Close stops a build that is running and waits until it has ended. No
// build starts afterwards.
func (s *Server) Close() {
	s.mu.Lock()
	s.stop()
	s.mu.Unlock()
	s.builds.Wait()
}

// sweep removes what an interrupted build left behind.
func (s *Server) sweep() {
	entries, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		s.log.Error("read the release directory", "err", err)
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			if err := s.root.RemoveAll(e.Name()); err != nil {
				s.log.Warn("remove a leftover", "name", e.Name(), "err", err)
			}
		}
	}
}

// answer is what a handler decided: a refusal with its status and one line
// of text, or (the zero value) that it has written the response itself.
type answer struct {
	status int
	text   string
}

var ok = answer{}

func refuse(status int, text string) answer { return answer{status, text} }

// recorder remembers the status a response went out with, for the log.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(p)
}

// signed wraps a handler: the request must carry the group's signature, and
// the names in its path must be a release and a target.
func (s *Server) signed(next func(w http.ResponseWriter, r *http.Request) answer) http.HandlerFunc {
	return func(plain http.ResponseWriter, r *http.Request) {
		w := &recorder{ResponseWriter: plain}
		a := func() answer {
			bodySHA, err := updates.VerifyRequest(r, s.cfg.Secret, s.cfg.Now())
			if err != nil {
				return refuse(http.StatusUnauthorized, err.Error())
			}
			if v := r.PathValue("version"); v != "" && !updates.IsRelease(v) {
				return refuse(http.StatusNotFound, "not a release: a version is X.Y.Z")
			}
			if t := r.PathValue("target"); t != "" && !updates.ValidTarget(t) {
				return refuse(http.StatusNotFound, "not a target: a target is <os>-<arch>")
			}
			if bodySHA != updates.EmptySHA || r.ContentLength > 0 {
				return refuse(http.StatusBadRequest, "no request to this server has a body")
			}
			return next(w, r)
		}()
		switch {
		case a.text != "":
			http.Error(w, a.text, a.status)
		case w.status == 0:
			// Nothing to say beyond "done".
			w.WriteHeader(http.StatusOK)
		}
		level := slog.LevelInfo
		switch {
		case w.status >= 500:
			level = slog.LevelError
		case w.status == http.StatusUnauthorized:
			// Anyone can send these to a public address; they are told
			// why, and the log is kept for what the group did.
			level = slog.LevelDebug
		}
		s.log.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.EscapedPath(), "status", w.status, "reason", a.text)
	}
}

// fail is the answer to an error that is the server's own.
func (s *Server) fail(what string, err error) answer {
	s.log.Error(what, "err", err)
	return refuse(http.StatusInternalServerError, "the server could not "+what)
}

// readManifest returns the published manifest of version.
func (s *Server) readManifest(version string) (raw []byte, m updates.Manifest, err error) {
	if raw, err = s.root.ReadFile(version + "/" + manifestName); err != nil {
		return nil, m, err
	}
	if err = json.Unmarshal(raw, &m); err != nil {
		return nil, m, err
	}
	return raw, m, nil
}

func (s *Server) serveManifest(w http.ResponseWriter, version string) answer {
	raw, _, err := s.readManifest(version)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return refuse(http.StatusNotFound, "no such release")
	case err != nil:
		return s.fail("read the manifest", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(raw)
	return ok
}

func (s *Server) latestVersion() (string, error) {
	raw, err := s.root.ReadFile(latestName)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func (s *Server) latest(w http.ResponseWriter, _ *http.Request) answer {
	version, err := s.latestVersion()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return refuse(http.StatusNotFound, "no release yet")
	case err != nil:
		return s.fail("read the latest release", err)
	}
	return s.serveManifest(w, version)
}

func (s *Server) manifest(w http.ResponseWriter, r *http.Request) answer {
	return s.serveManifest(w, r.PathValue("version"))
}

func (s *Server) program(w http.ResponseWriter, r *http.Request) answer {
	version, target := r.PathValue("version"), r.PathValue("target")
	_, m, err := s.readManifest(version)
	if err == nil {
		if _, listed := m.Files[target]; !listed {
			err = fs.ErrNotExist
		}
	}
	var f *os.File
	if err == nil {
		f, err = s.root.Open(version + "/" + target)
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return refuse(http.StatusNotFound, "no such program in a published release")
	case err != nil:
		return s.fail("read the program", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return s.fail("read the program", err)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", info.ModTime(), f)
	return ok
}

func (s *Server) discard(name string) {
	if err := s.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Warn("remove a leftover", "name", name, "err", err)
	}
}

// replace writes a small file so that a reader sees the old or the new one.
func (s *Server) replace(name string, data []byte) error {
	tmp := tmpPrefix + strings.ReplaceAll(name, "/", "-")
	if err := s.root.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := s.root.Rename(tmp, name); err != nil {
		s.discard(tmp)
		return err
	}
	return nil
}

// say answers with the state of a build.
func (s *Server) say(w http.ResponseWriter, status int, b updates.Build) answer {
	raw, err := json.Marshal(b)
	if err != nil {
		return s.fail("write the answer", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(raw)
	return ok
}

// release starts the build of a release, unless it is there already.
func (s *Server) release(w http.ResponseWriter, r *http.Request) answer {
	version := r.PathValue("version")
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, err := s.latestVersion()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return s.fail("read the latest release", err)
	}
	newest := err != nil || updates.Older(latest, version)
	switch _, _, err := s.readManifest(version); {
	case err == nil:
		// Built before, and a release never changes. If that run stopped
		// before the release became the latest, finish that.
		if newest {
			if err := s.replace(latestName, []byte(version+"\n")); err != nil {
				return s.fail("store the latest release", err)
			}
		}
		return s.say(w, http.StatusOK, updates.Build{Version: version, State: updates.Published})
	case !errors.Is(err, fs.ErrNotExist):
		return s.fail("read the manifest", err)
	}
	switch {
	case s.ctx.Err() != nil:
		return refuse(http.StatusServiceUnavailable, "the server is stopping")
	case s.building == version:
		return s.say(w, http.StatusAccepted, s.last)
	case s.building != "":
		return refuse(http.StatusConflict, "release "+s.building+" is being built; ask again when it is done")
	case !newest:
		return refuse(http.StatusConflict, "release "+version+" is not newer than the latest, "+latest)
	}
	s.building, s.last = version, updates.Build{Version: version, State: updates.Building}
	s.builds.Add(1)
	go s.build(version)
	return s.say(w, http.StatusAccepted, s.last)
}

// state says how the build of a release is going.
func (s *Server) state(w http.ResponseWriter, r *http.Request) answer {
	version := r.PathValue("version")
	s.mu.Lock()
	last := s.last
	s.mu.Unlock()
	switch _, _, err := s.readManifest(version); {
	case err == nil:
		return s.say(w, http.StatusOK, updates.Build{Version: version, State: updates.Published})
	case !errors.Is(err, fs.ErrNotExist):
		return s.fail("read the manifest", err)
	case last.Version != version:
		return refuse(http.StatusNotFound, "no build of release "+version+" is known")
	}
	return s.say(w, http.StatusOK, last)
}

// build builds one release and records how it went.
func (s *Server) build(version string) {
	defer s.builds.Done()
	s.log.Info("building", "version", version)
	began := time.Now()
	m, err := s.make(version)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.building = ""
	if err != nil {
		s.last = updates.Build{Version: version, State: updates.Failed, Error: end(err.Error())}
		s.log.Error("the build failed", "version", version, "err", err)
		return
	}
	s.last = updates.Build{Version: version, State: updates.Published}
	s.log.Info("published", "version", version, "files", len(m.Files), "took", time.Since(began).Round(time.Millisecond))
}

// end is the end of text, at most maxReason bytes of it.
func end(text string) string {
	if len(text) > maxReason {
		text = strings.ToValidUTF8(text[len(text)-maxReason:], "")
	}
	return text
}

// make builds the programs of a release in a temporary directory inside
// the root, then stores and signs them. A build that is cut off leaves only
// that directory, which the next start removes.
func (s *Server) make(version string) (updates.Manifest, error) {
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.Limit)
	defer cancel()
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return updates.Manifest{}, err
	}
	work := tmpPrefix + "build-" + hex.EncodeToString(id[:])
	if err := s.root.Mkdir(work, 0o700); err != nil {
		return updates.Manifest{}, err
	}
	defer func() {
		if err := s.root.RemoveAll(work); err != nil {
			s.log.Warn("remove the build directory", "name", work, "err", err)
		}
	}()
	if err := s.cfg.Build(ctx, version, filepath.Join(s.root.Name(), work)); err != nil {
		return updates.Manifest{}, err
	}
	return s.store(version, work)
}

// store takes the programs a build left in the directory work into release
// version, signs the manifest and makes the release the latest.
func (s *Server) store(version, work string) (updates.Manifest, error) {
	entries, err := fs.ReadDir(s.root.FS(), work)
	if err != nil {
		return updates.Manifest{}, err
	}
	if len(entries) == 0 {
		return updates.Manifest{}, errors.New("the build left no program")
	}
	for _, e := range entries {
		if !updates.ValidTarget(e.Name()) || !e.Type().IsRegular() {
			return updates.Manifest{}, fmt.Errorf("the build left %q, which is not a program named <os>-<arch>", e.Name())
		}
	}
	// Nothing reads a release without a manifest, and one build runs at a
	// time, so the programs are moved in without the lock.
	if err := s.root.Mkdir(version, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return updates.Manifest{}, err
	}
	files := make(map[string]updates.File, len(entries))
	for _, e := range entries {
		target := e.Name()
		file, err := s.take(work+"/"+target, version+"/"+target)
		if err != nil {
			return updates.Manifest{}, fmt.Errorf("store %s: %w", target, err)
		}
		files[target] = file
	}
	m := updates.Seal(s.cfg.Key, version, files, s.cfg.Now().Unix())
	raw, err := json.Marshal(m)
	if err != nil {
		return updates.Manifest{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.replace(version+"/"+manifestName, raw); err != nil {
		return updates.Manifest{}, fmt.Errorf("store the manifest: %w", err)
	}
	if err := s.replace(latestName, []byte(version+"\n")); err != nil {
		return updates.Manifest{}, fmt.Errorf("store the latest release: %w", err)
	}
	return m, nil
}

// take moves the program a build left at from into the release as name and
// returns its size and digest.
func (s *Server) take(from, name string) (updates.File, error) {
	f, err := s.root.OpenFile(from, os.O_RDWR, 0)
	if err != nil {
		return updates.File{}, err
	}
	sum := sha256.New()
	// One byte more than allowed shows a program that is too large.
	size, err := io.Copy(sum, io.LimitReader(f, updates.MaxProgram+1))
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	switch {
	case err != nil:
		return updates.File{}, err
	case size == 0:
		return updates.File{}, errors.New("the program is empty")
	case size > updates.MaxProgram:
		return updates.File{}, fmt.Errorf("the program is over the limit of %d bytes", int64(updates.MaxProgram))
	}
	if err := s.root.Rename(from, name); err != nil {
		return updates.File{}, err
	}
	return updates.File{SHA256: hex.EncodeToString(sum.Sum(nil)), Size: size}, nil
}
