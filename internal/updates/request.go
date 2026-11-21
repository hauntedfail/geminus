package updates

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/hauntedfail/geminus/internal/wire"
)

// Path is where the releases are on the server, and DefaultBase the server
// every geminus asks unless EnvBase names another.
const (
	Path        = "/geminus/updates"
	DefaultBase = "https://sibyl.haunted.fail" + Path
	EnvBase     = "GEMINUS_UPDATES_URL"
	// MaxProgram is the largest program a release may hold.
	MaxProgram = 32 << 20
)

// Every request to the server is signed, reads too: the programs are private
// and the address is public.
const (
	// HeaderTime is when the request was signed, in Unix seconds.
	HeaderTime = "Geminus-Time"
	// HeaderBody is the SHA-256 of the request body the signature covers.
	HeaderBody = "Geminus-Body-SHA256"
	// HeaderSignature is the signature.
	HeaderSignature = "Geminus-Signature"
	// Window is how far the signing time may be from the verifier's clock.
	Window = 5 * time.Minute
)

// EmptySHA is the SHA-256 of no body.
var EmptySHA = hex.EncodeToString(sha256.New().Sum(nil))

// Why a request is refused.
var (
	ErrUnsigned  = errors.New("the request is not signed")
	ErrClock     = errors.New("the request was signed more than five minutes from this computer's time: check both clocks")
	ErrSignature = errors.New("the signature is not this group's")
)

func requestLabel(method, path, bodySHA string) string {
	return "geminus-updates-request:" + method + ":" + path + ":" + bodySHA
}

// SignRequest signs req for a body with the given SHA-256 (EmptySHA for
// none).
func SignRequest(req *http.Request, secret []byte, bodySHA string, now time.Time) {
	ts := now.Unix()
	req.Header.Set(HeaderTime, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderBody, bodySHA)
	req.Header.Set(HeaderSignature, wire.Sign(secret, "", requestLabel(req.Method, req.URL.EscapedPath(), bodySHA), ts))
}

// VerifyRequest checks the signature of req and returns the SHA-256 of the
// body it covers. Whoever reads the body has to check it against that.
func VerifyRequest(req *http.Request, secret []byte, now time.Time) (bodySHA string, err error) {
	sig, bodySHA := req.Header.Get(HeaderSignature), req.Header.Get(HeaderBody)
	ts, err := strconv.ParseInt(req.Header.Get(HeaderTime), 10, 64)
	if err != nil || sig == "" || !hashPattern.MatchString(bodySHA) {
		return "", ErrUnsigned
	}
	if !wire.Verify(secret, "", requestLabel(req.Method, req.URL.EscapedPath(), bodySHA), ts, sig) {
		return "", ErrSignature
	}
	if d := now.Sub(time.Unix(ts, 0)); d > Window || d < -Window {
		return "", ErrClock
	}
	return bodySHA, nil
}
