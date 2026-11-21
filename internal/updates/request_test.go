package updates

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func signed(t *testing.T, method, url, body string, at time.Time) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	SignRequest(req, secret, body, at)
	return req
}

func TestASignedRequestVerifiesAndSaysWhichBodyItCovers(t *testing.T) {
	now := time.Unix(1795300000, 0)
	req := signed(t, "PUT", "https://sibyl.example/geminus/updates/0.5.0/linux-amd64", hash('a'), now)
	got, err := VerifyRequest(req, secret, now.Add(299*time.Second))
	if err != nil || got != hash('a') {
		t.Fatalf("VerifyRequest = %q, %v", got, err)
	}
	read := signed(t, "GET", "https://sibyl.example/geminus/updates/latest", EmptySHA, now)
	if got, err := VerifyRequest(read, secret, now.Add(-299*time.Second)); err != nil || got != EmptySHA {
		t.Fatalf("VerifyRequest = %q, %v", got, err)
	}
}

func TestARequestIsGoodOnlyForItsMethodPathBodyAndTime(t *testing.T) {
	now := time.Unix(1795300000, 0)
	const url = "https://sibyl.example/geminus/updates/0.5.0/linux-amd64"
	for name, c := range map[string]struct {
		change func(*http.Request)
		at     time.Time
		want   error
	}{
		"another method": {func(r *http.Request) { r.Method = "GET" }, now, ErrSignature},
		"another path":   {func(r *http.Request) { r.URL.Path = "/geminus/updates/0.5.0/windows-amd64" }, now, ErrSignature},
		"another body":   {func(r *http.Request) { r.Header.Set(HeaderBody, hash('b')) }, now, ErrSignature},
		"another time":   {func(r *http.Request) { r.Header.Set(HeaderTime, "1795300001") }, now, ErrSignature},
		"no signature":   {func(r *http.Request) { r.Header.Del(HeaderSignature) }, now, ErrUnsigned},
		"no time":        {func(r *http.Request) { r.Header.Del(HeaderTime) }, now, ErrUnsigned},
		"no body hash":   {func(r *http.Request) { r.Header.Del(HeaderBody) }, now, ErrUnsigned},
		"a time of text": {func(r *http.Request) { r.Header.Set(HeaderTime, "soon") }, now, ErrUnsigned},
		"a late clock":   {func(*http.Request) {}, now.Add(301 * time.Second), ErrClock},
		"an early clock": {func(*http.Request) {}, now.Add(-301 * time.Second), ErrClock},
	} {
		req := signed(t, "PUT", url, hash('a'), now)
		c.change(req)
		if _, err := VerifyRequest(req, secret, c.at); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	req := signed(t, "PUT", url, hash('a'), now)
	if _, err := VerifyRequest(req, []byte("ffffffffffffffffffffffffffffffff"), now); !errors.Is(err, ErrSignature) {
		t.Errorf("another secret: err = %v", err)
	}
}
