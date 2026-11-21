// Package server hands out geminus releases from a directory. It is the
// whole of cmd/geminus-latest-channel apart from flags and the listener.
//
//	GET /geminus/updates/latest              the manifest of the newest release
//	GET /geminus/updates/{version}           the manifest of that release
//	GET /geminus/updates/{version}/{target}  one program
//	PUT /geminus/updates/{version}/{target}  upload one program
//	PUT /geminus/updates/{version}           publish: upload the manifest
//
// Every request is signed with the group's secret (package updates). A
// release is visible once its manifest is published, and never changes
// afterwards.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hauntedfail/geminus/internal/updates"
)

const (
	// maxProgram and maxManifest bound an upload.
	maxProgram  = updates.MaxProgram
	maxManifest = 64 << 10

	manifestName = "manifest.json"
	latestName   = "latest"
	tmpPrefix    = ".tmp-"
)

type server struct {
	root   *os.Root
	secret []byte
	now    func() time.Time
	log    *slog.Logger
	// mu serializes everything that changes the directory.
	mu sync.Mutex
}

// New returns the handler for the releases kept in root.
func New(root *os.Root, secret []byte, now func() time.Time, log *slog.Logger) http.Handler {
	s := &server{root: root, secret: secret, now: now, log: log}
	s.sweep()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+updates.Path+"/latest", s.signed(s.latest))
	mux.HandleFunc("GET "+updates.Path+"/{version}", s.signed(s.manifest))
	mux.HandleFunc("GET "+updates.Path+"/{version}/{target}", s.signed(s.program))
	mux.HandleFunc("PUT "+updates.Path+"/{version}/{target}", s.signed(s.upload))
	mux.HandleFunc("PUT "+updates.Path+"/{version}", s.signed(s.publish))
	return mux
}

// sweep removes what an interrupted upload left behind.
func (s *server) sweep() {
	entries, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		s.log.Error("read the release directory", "err", err)
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			if err := s.root.Remove(e.Name()); err != nil {
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
func (s *server) signed(next func(w http.ResponseWriter, r *http.Request, bodySHA string) answer) http.HandlerFunc {
	return func(plain http.ResponseWriter, r *http.Request) {
		w := &recorder{ResponseWriter: plain}
		a := func() answer {
			bodySHA, err := updates.VerifyRequest(r, s.secret, s.now())
			if err != nil {
				return refuse(http.StatusUnauthorized, err.Error())
			}
			if v := r.PathValue("version"); v != "" && !updates.IsRelease(v) {
				return refuse(http.StatusNotFound, "not a release: a version is X.Y.Z")
			}
			if t := r.PathValue("target"); t != "" && !updates.ValidTarget(t) {
				return refuse(http.StatusNotFound, "not a target: a target is <os>-<arch>")
			}
			if r.Method == http.MethodGet && bodySHA != updates.EmptySHA {
				return refuse(http.StatusBadRequest, "a read has no body")
			}
			return next(w, r, bodySHA)
		}()
		switch {
		case a.text != "":
			http.Error(w, a.text, a.status)
		case w.status == 0:
			// Nothing to say beyond "done".
			w.WriteHeader(http.StatusOK)
		}
		level := slog.LevelInfo
		if w.status >= 500 {
			level = slog.LevelError
		}
		s.log.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.EscapedPath(), "status", w.status, "reason", a.text)
	}
}

// fail is the answer to an error that is the server's own.
func (s *server) fail(what string, err error) answer {
	s.log.Error(what, "err", err)
	return refuse(http.StatusInternalServerError, "the server could not "+what)
}

// readManifest returns the published manifest of version.
func (s *server) readManifest(version string) (raw []byte, m updates.Manifest, err error) {
	if raw, err = s.root.ReadFile(version + "/" + manifestName); err != nil {
		return nil, m, err
	}
	if err = json.Unmarshal(raw, &m); err != nil {
		return nil, m, err
	}
	return raw, m, nil
}

func (s *server) serveManifest(w http.ResponseWriter, version string) answer {
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

func (s *server) latestVersion() (string, error) {
	raw, err := s.root.ReadFile(latestName)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func (s *server) latest(w http.ResponseWriter, _ *http.Request, _ string) answer {
	version, err := s.latestVersion()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return refuse(http.StatusNotFound, "no release yet")
	case err != nil:
		return s.fail("read the latest release", err)
	}
	return s.serveManifest(w, version)
}

func (s *server) manifest(w http.ResponseWriter, r *http.Request, _ string) answer {
	return s.serveManifest(w, r.PathValue("version"))
}

func (s *server) program(w http.ResponseWriter, r *http.Request, _ string) answer {
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

// receive stores the request body in a temporary file and returns its name,
// after checking it is the body the request was signed for.
func (s *server) receive(w http.ResponseWriter, r *http.Request, bodySHA string, limit int64) (name string, a answer) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", s.fail("name an upload", err)
	}
	name = tmpPrefix + hex.EncodeToString(id[:])
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", s.fail("store the upload", err)
	}
	sum := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, sum), http.MaxBytesReader(w, r.Body, limit))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		a = refuse(http.StatusRequestEntityTooLarge, "the upload is over the limit")
	case err != nil:
		a = refuse(http.StatusBadRequest, "the upload did not arrive whole")
	case hex.EncodeToString(sum.Sum(nil)) != bodySHA:
		a = refuse(http.StatusBadRequest, "the body is not the one the request was signed for")
	default:
		return name, ok
	}
	s.discard(name)
	return "", a
}

func (s *server) discard(name string) {
	if err := s.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Warn("remove an upload", "name", name, "err", err)
	}
}

func (s *server) upload(w http.ResponseWriter, r *http.Request, bodySHA string) answer {
	version, target := r.PathValue("version"), r.PathValue("target")
	tmp, a := s.receive(w, r, bodySHA, maxProgram)
	if a != ok {
		return a
	}
	defer s.discard(tmp)
	s.mu.Lock()
	defer s.mu.Unlock()
	// A published release never changes; the same bytes again are fine.
	switch _, m, err := s.readManifest(version); {
	case err == nil && m.Files[target].SHA256 == bodySHA:
		return ok
	case err == nil:
		return refuse(http.StatusConflict, "release "+version+" is published and cannot be changed")
	case !errors.Is(err, fs.ErrNotExist):
		return s.fail("read the manifest", err)
	}
	if err := s.root.Mkdir(version, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return s.fail("store the upload", err)
	}
	if err := s.root.Rename(tmp, version+"/"+target); err != nil {
		return s.fail("store the upload", err)
	}
	w.WriteHeader(http.StatusCreated)
	return ok
}

// fileMatches reports whether the stored program is the one the manifest
// names.
func (s *server) fileMatches(name string, want updates.File) (bool, error) {
	f, err := s.root.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	sum := sha256.New()
	n, err := io.Copy(sum, f)
	if err != nil {
		return false, err
	}
	return n == want.Size && hex.EncodeToString(sum.Sum(nil)) == want.SHA256, nil
}

// replace writes a small file so that a reader sees the old or the new one.
func (s *server) replace(name string, data []byte) error {
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

func (s *server) publish(w http.ResponseWriter, r *http.Request, bodySHA string) answer {
	version := r.PathValue("version")
	tmp, a := s.receive(w, r, bodySHA, maxManifest)
	if a != ok {
		return a
	}
	defer s.discard(tmp)
	raw, err := s.root.ReadFile(tmp)
	if err != nil {
		return s.fail("read the upload", err)
	}
	var m updates.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return refuse(http.StatusBadRequest, "the manifest is not JSON")
	}
	if m.Version != version || !m.Verify(s.secret) {
		return refuse(http.StatusBadRequest, "the manifest is not a signed manifest of release "+version)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, err := s.latestVersion()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return s.fail("read the latest release", err)
	}
	newest := err != nil || updates.Older(latest, version)
	switch _, old, err := s.readManifest(version); {
	case err == nil && maps.Equal(old.Files, m.Files):
		// Published before with the same programs: a release run again.
		// The manifest stays as it was first signed. If the earlier run
		// stopped before the release became the latest, finish that.
		if newest {
			if err := s.replace(latestName, []byte(version+"\n")); err != nil {
				return s.fail("store the latest release", err)
			}
		}
		return ok
	case err == nil:
		return refuse(http.StatusConflict, "release "+version+" is published and cannot be changed")
	case !errors.Is(err, fs.ErrNotExist):
		return s.fail("read the manifest", err)
	}
	if !newest {
		return refuse(http.StatusConflict, "release "+version+" is not newer than the latest, "+latest)
	}
	for target, want := range m.Files {
		switch same, err := s.fileMatches(version+"/"+target, want); {
		case err != nil:
			return s.fail("read the program", err)
		case !same:
			return refuse(http.StatusBadRequest, "the uploaded "+target+" is missing or is not the file the manifest names")
		}
	}
	if err := s.replace(version+"/"+manifestName, raw); err != nil {
		return s.fail("store the manifest", err)
	}
	if err := s.replace(latestName, []byte(version+"\n")); err != nil {
		return s.fail("store the latest release", err)
	}
	s.log.Info("published", "version", version, "files", len(m.Files))
	w.WriteHeader(http.StatusCreated)
	return ok
}
