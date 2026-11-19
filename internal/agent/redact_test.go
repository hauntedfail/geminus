package agent

import (
	"reflect"
	"strings"
	"testing"
)

// Quoted values and headers, including the Phase 3 A-1 regression cases.
func TestRedactArgvQuotedAndHeaderValues(t *testing.T) {
	const r = Redacted
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		// Regression cases.
		{"double quoted assignment in a shell command", []string{"sh", "-c", `API_KEY="s3cr3t" cmd`}, []string{"sh", "-c", `API_KEY="` + r + `" cmd`}},
		{"double quoted assignment after echo", []string{"sh", "-c", `echo API_KEY="s3cr3t"`}, []string{"sh", "-c", `echo API_KEY="` + r + `"`}},
		{"single quoted assignment in a shell command", []string{"sh", "-c", `TOKEN='x' cmd`}, []string{"sh", "-c", `TOKEN='` + r + `' cmd`}},
		{"single quoted assignment as a whole argument", []string{"env", `TOKEN='x'`, "cmd"}, []string{"env", "TOKEN=" + r, "cmd"}},
		{"api key header", []string{"curl", "-H", "X-Api-Key: abc123", "https://example.com"}, []string{"curl", "-H", "X-Api-Key: " + r, "https://example.com"}},

		// Three forms of assignment.
		{"quoted value with spaces", []string{"sh", "-c", `DB_PASSWORD="my pass word" psql`}, []string{"sh", "-c", `DB_PASSWORD="` + r + `" psql`}},
		{"single quoted value with spaces", []string{"sh", "-c", `DB_PASSWORD='my pass word' psql`}, []string{"sh", "-c", `DB_PASSWORD='` + r + `' psql`}},
		{"escaped quote inside a double quoted value", []string{"sh", "-c", `TOKEN="a\"b c" run`}, []string{"sh", "-c", `TOKEN="` + r + `" run`}},
		{"export with a quoted value", []string{"bash", "-lc", `export GH_TOKEN="ghp_abc"; gh pr list`}, []string{"bash", "-lc", `export GH_TOKEN="` + r + `"; gh pr list`}},
		{"assignment inside outer quotes", []string{"sh", "-c", `echo 'TOKEN=abc' 'next word'`}, []string{"sh", "-c", `echo 'TOKEN=` + r + `' 'next word'`}},
		{"quoted assignment inside outer quotes", []string{"sh", "-c", `ssh host 'API_KEY="abc" run' 'next word'`}, []string{"sh", "-c", `ssh host 'API_KEY="` + r + `" run' 'next word'`}},
		{"following quoted argument is kept", []string{"sh", "-c", `curl -d "token=abc" "https://example.com/x"`}, []string{"sh", "-c", `curl -d "token=` + r + `" "https://example.com/x"`}},
		{"value continued by an adjacent quoted part", []string{"sh", "-c", `TOKEN=abc"def"'ghi' run`}, []string{"sh", "-c", "TOKEN=" + r + " run"}},
		{"quoted value continued by an adjacent part", []string{"sh", "-c", `TOKEN="abc"def run`}, []string{"sh", "-c", `TOKEN="` + r + `" run`}},
		{"unterminated quote", []string{"sh", "-c", `echo TOKEN="abc`}, []string{"sh", "-c", `echo TOKEN="` + r}},
		{"backslash escaped quotes", []string{"bash", "-c", `ssh host "API_KEY=\"s3cr3t\" cmd"`}, []string{"bash", "-c", `ssh host "API_KEY=\"` + r + `\" cmd"`}},
		{"empty quoted value is kept", []string{"sh", "-c", `TOKEN="" cmd`}, []string{"sh", "-c", `TOKEN="" cmd`}},
		{"flag with a quoted value inside a shell command", []string{"sh", "-c", `tool --api-key="abc def" --db-password='p q' run`}, []string{"sh", "-c", `tool --api-key="` + r + `" --db-password='` + r + `' run`}},
		{"leading assignment with a name only the flag list knows", []string{"sh", "-c", `MY_AUTH="abc def" MY_CREDENTIALS=x run`}, []string{"sh", "-c", `MY_AUTH="` + r + `" MY_CREDENTIALS=x run`}},
		{"url query", []string{"curl", "https://example.com/?api_key=abc123"}, []string{"curl", "https://example.com/?api_key=" + r}},

		// AWS-style key names.
		{"aws secret access key", []string{"env", "AWS_SECRET_ACCESS_KEY=abc/def", "AWS_ACCESS_KEY_ID=AKIA", "aws"}, []string{"env", "AWS_SECRET_ACCESS_KEY=" + r, "AWS_ACCESS_KEY_ID=AKIA", "aws"}},
		{"secret key flags", []string{"x", "--secret-key", "a", "--access-key=b"}, []string{"x", "--secret-key", r, "--access-key=" + r}},
		{"aws key inside a shell command", []string{"sh", "-c", `AWS_SECRET_ACCESS_KEY="abc" aws s3 ls`}, []string{"sh", "-c", `AWS_SECRET_ACCESS_KEY="` + r + `" aws s3 ls`}},

		// Headers.
		{"api key header inside a shell command", []string{"sh", "-c", `curl -H 'X-Api-Key: abc123' https://example.com`}, []string{"sh", "-c", `curl -H 'X-Api-Key: ` + r + `' https://example.com`}},
		{"double quoted header inside a shell command", []string{"sh", "-c", `curl -H "x-api-key: abc123" https://example.com`}, []string{"sh", "-c", `curl -H "x-api-key: ` + r + `" https://example.com`}},
		{"header without a space", []string{"curl", "-H", "X-Auth-Token:abc123"}, []string{"curl", "-H", "X-Auth-Token:" + r}},
		{"unquoted header inside a shell command", []string{"sh", "-c", "http GET example.com X-Api-Key:abc123 page==2"}, []string{"sh", "-c", "http GET example.com X-Api-Key:" + r + " page==2"}},
		{"other secret headers", []string{"curl", "-H", "Private-Token: glpat-1", "-H", "X-Vault-Token: hvs.1", "-H", "Api-Key: k"}, []string{"curl", "-H", "Private-Token: " + r, "-H", "X-Vault-Token: " + r, "-H", "Api-Key: " + r}},
		{"authorization without a scheme", []string{"curl", "-H", "Authorization: abc123"}, []string{"curl", "-H", "Authorization: " + r}},
		{"authorization without a scheme inside a shell command", []string{"sh", "-c", `curl -H 'Authorization: abc123' https://example.com`}, []string{"sh", "-c", `curl -H 'Authorization: ` + r + `' https://example.com`}},
		{"authorization with the token scheme", []string{"curl", "-H", "Authorization: token ghp_abc"}, []string{"curl", "-H", "Authorization: token " + r}},
		{"bearer inside quotes keeps the closing quote", []string{"sh", "-c", `curl -H "Authorization: Bearer abc.def" https://example.com`}, []string{"sh", "-c", `curl -H "Authorization: Bearer ` + r + `" https://example.com`}},
		{"proxy authorization keeps the scheme", []string{"curl", "-H", "Proxy-Authorization: Basic dXNlcjpwdw=="}, []string{"curl", "-H", "Proxy-Authorization: Basic " + r}},
		{"json body", []string{"curl", "-d", `{"api_key": "abc123", "name": "bob"}`}, []string{"curl", "-d", `{"api_key": "` + r + `", "name": "bob"}`}},
		{"json body with a compound name", []string{"curl", "-d", `{"client_secret":"abc 123","page":2}`}, []string{"curl", "-d", `{"client_secret":"` + r + `","page":2}`}},

		// Cases identified during review.
		{"authorization with another scheme", []string{"curl", "-H", "Authorization: ApiKey abc123"}, []string{"curl", "-H", "Authorization: ApiKey " + r}},
		{"authorization with an unknown scheme inside a shell command", []string{"sh", "-c", `curl -H 'Authorization: Custom abc123' https://example.com`}, []string{"sh", "-c", `curl -H 'Authorization: Custom ` + r + `' https://example.com`}},
		{"bearer with a quoted token", []string{"claude", `send Authorization: Bearer "abc123" to the api`}, []string{"claude", `send Authorization: Bearer "` + r + `" to the api`}},
		{"digest authorization", []string{"curl", "-H", `Authorization: Digest username="u", response="abc123"`}, []string{"curl", "-H", "Authorization: Digest " + r}},
		{"json body with a bare key", []string{"curl", "-d", `{"token":"abc123","password": "hunter2","user":"bob"}`}, []string{"curl", "-d", `{"token":"` + r + `","password": "` + r + `","user":"bob"}`}},
		{"single quoted bare key", []string{"python3", "-c", `post({'token': 'abc123'})`}, []string{"python3", "-c", `post({'token': '` + r + `'})`}},
		{"escaped quotes with a space as a whole argument", []string{"ssh", "host", `TOKEN=\"abc def\" cmd`}, []string{"ssh", "host", `TOKEN=\"` + r + `\" cmd`}},
		{"backslash escaped space", []string{"sh", "-c", `TOKEN=a\ b cmd`}, []string{"sh", "-c", "TOKEN=" + r + " cmd"}},
		{"ansi-c quoting", []string{"bash", "-c", `TOKEN=$'abc def' cmd`}, []string{"bash", "-c", `TOKEN=$'` + r + `' cmd`}},
		{"url password with an at sign", []string{"git", "clone", "https://user:p@ss@example.com/repo.git"}, []string{"git", "clone", "https://user:" + r + "@example.com/repo.git"}},
		{"pwd and pass names", []string{"env", "MYSQL_PWD=a", "DB_PASS=b", "PASS=c", "x", "--db-pass", "d", "--pass=e"}, []string{"env", "MYSQL_PWD=" + r, "DB_PASS=" + r, "PASS=" + r, "x", "--db-pass", r, "--pass=" + r}},
		{"pwd and pass inside a shell command", []string{"sh", "-c", `MYSQL_PWD="a b" DB_PASS=c mysql`}, []string{"sh", "-c", `MYSQL_PWD="` + r + `" DB_PASS=` + r + ` mysql`}},
		{"names that only look like pass or pwd", []string{"x", "--bypass", "1", "--compass=n", "PWD=/tmp", "--pwd", "/tmp", "OLDPWD=/"}, []string{"x", "--bypass", "1", "--compass=n", "PWD=/tmp", "--pwd", "/tmp", "OLDPWD=/"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := append([]string(nil), tt.in...)
			got := RedactArgv(in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RedactArgv = %q\nwant         %q", got, tt.want)
			}
			if !reflect.DeepEqual(in, tt.in) {
				t.Errorf("input was modified: %q", in)
			}
		})
	}
}

// Leave ordinary prose and code fragments unchanged.
func TestRedactArgvKeepsOrdinaryText(t *testing.T) {
	texts := []string{
		"the token is in the vault",
		"SyntaxError: Unexpected token: } in JSON at position 3",
		"explain authorization: how does it differ from authentication?",
		"authorization: handled by the middleware",
		"here is the secret: never use globals",
		"forgot password: what should the page say?",
		"set max_tokens=100 and tokenizer=bpe, keyboard=us",
		"--max-tokens: 100, --tokenizer: bpe",
		"primary_key: id, foreign_key: user_id",
		"const token = getToken(); if (token == null) return",
		"fetch(url, {credentials: 'include'})",
		"set oauth=1 and author=bob",
		"if (token==null) return; password===undefined",
		"don't touch the 'token' column; it's \"secret\" stuff",
		"at 12:30 the api key: rotates",
		"a=b c: d",
		"https://example.com:8443/a@b?page=2",
		`TOKEN="" and PASSWORD=`,
		"results: pass=3 fail=2, bypass=1",
		`the "token" field and the 'password' column: both are strings`,
		`{"tokens": 100, "max_tokens": "200", "tokenizer": "bpe"}`,
	}
	for _, text := range texts {
		t.Run(text, func(t *testing.T) {
			got := RedactArgv([]string{"claude", text})
			if want := []string{"claude", text}; !reflect.DeepEqual(got, want) {
				t.Errorf("RedactArgv = %q\nwant         %q", got, want)
			}
		})
	}
}

// The value itself must never survive, regardless of how the replacement looks.
func TestRedactArgvNeverLeaksTheValue(t *testing.T) {
	const secret = "s3cr3tVALUE"
	shapes := []string{
		`API_KEY=%s`,
		`API_KEY="%s"`,
		`API_KEY='%s'`,
		`export API_KEY="%s"`,
		`echo API_KEY="%s"`,
		`API_KEY="%s" cmd`,
		`API_KEY='%s' cmd`,
		`cmd; API_KEY="%s" cmd2`,
		`API_KEY=\"%s\" cmd`,
		`API_KEY="pre %s post" cmd`,
		`API_KEY=pre"%s"post cmd`,
		`API_KEY="pre"%s cmd`,
		`A_TOKEN=x"%s" B_TOKEN='%s' cmd`,
		`curl -H 'X-Api-Key: %s' https://example.com`,
		`curl -H "X-Api-Key: %s" https://example.com`,
		`curl -H X-Api-Key:%s https://example.com`,
		`curl -H 'Authorization: %s' https://example.com`,
		`curl -H 'Authorization: Bearer %s' https://example.com`,
		`curl -H "Authorization: Basic %s" https://example.com`,
		`curl -H 'Authorization: token %s'`,
		`curl -d '{"api_key":"%s"}' https://example.com`,
		`curl -d '{"access_token": "%s"}' https://example.com`,
		`curl https://user:%s@example.com/`,
		`curl 'https://example.com/?access_token=%s&page=2'`,
		`tool --password="%s" run`,
		`tool --api-key='%s' run`,
		`curl -H 'Authorization: ApiKey %s' https://example.com`,
		`curl -H 'Authorization: Bearer "%s"'`,
		`curl -H "Authorization: Digest username=u, response=%s"`,
		`curl -d '{"token":"%s"}' https://example.com`,
		`curl -d "{\"password\": \"%s\"}" https://example.com`,
		`TOKEN=\"pre %s\" cmd`,
		`TOKEN=pre\ %s cmd`,
		`TOKEN=$'pre %s' cmd`,
		`git clone https://user:p@%s@example.com/repo.git`,
		`MYSQL_PWD=%s mysql`,
	}
	for _, shape := range shapes {
		script := strings.ReplaceAll(shape, "%s", secret)
		t.Run(shape, func(t *testing.T) {
			for _, argv := range [][]string{{"sh", "-c", script}, {script}, {"claude", "run this: " + script}} {
				got := strings.Join(RedactArgv(argv), "\x00")
				if strings.Contains(got, secret) {
					t.Errorf("RedactArgv(%q) = %q, the secret is still there", argv, got)
				}
				if !strings.Contains(got, Redacted) {
					t.Errorf("RedactArgv(%q) = %q, want a %s marker", argv, got, Redacted)
				}
			}
		})
	}
}
