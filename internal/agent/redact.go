package agent

import (
	"regexp"
	"strings"
)

// Redacted replaces a value that has been withheld.
const Redacted = "[REDACTED]"

// Match sensitive key suffixes such as --api-key, --token, and --password, including joined
// forms like --accessToken. Suffix matching excludes --max-tokens and --tokenizer. Match
// pwd only after a separator (MYSQL_PWD), and pass after a separator or on its own
// (DB_PASS, --pass), excluding PWD and --bypass.
var secretKeyPattern = regexp.MustCompile(`(?i)(` + secretKeyNames + `|token|password|passwd|passphrase|secret|credentials?|auth|authorization|[-_]pwd|(?:^|[-_])pass)$`)

// Require a plausible flag or environment variable name so an equals sign in prompt text is
// not enough to trigger redaction.
var keyShapePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// These patterns match secrets embedded in a single argument, such as a cmd backend shell
// script or a curl header. Match more narrowly than for flag names because the argument may
// be ordinary prompt text.
const (
	// api-key / access_key / SECRET_ACCESS_KEY / private-key
	secretKeyNames = `(?:api|access|secret|private)[-_]?key`

	// Words that denote a secret even on their own.
	secretWords = secretKeyNames + `|token|secret|password|passwd|passphrase`

	// Names in NAME=value assignments. Exclude auth and credentials, common in prose or code
	// (oauth=1 or credentials: 'include'). Match pass and pwd only after a separator to avoid
	// redacting "pass=3 fail=2".
	assignmentName = `[A-Za-z0-9_]*(?:` + secretWords + `|_pwd|_pass)`

	// Names in Name: value headers. Require a compound with a separator (X-Api-Key,
	// X-Auth-Token, client_secret). Bare token, secret, and password also occur in prose such
	// as "Unexpected token: }" or "password: ...".
	headerName = `(?:[A-Za-z0-9_-]*` + secretKeyNames + `|[A-Za-z0-9_-]*[-_](?:token|secret|password|passwd|passphrase|authorization))`

	// Quoted values: escaped double quotes, double quotes, or single quotes (including $'...').
	// The first form occurs in `ssh host "API_KEY=\"x y\" cmd"`. Groups 2 through 7 capture
	// quote pairs (even for opening, odd for closing); group 1 belongs to the key.
	quotedValue = `(\\+["'])[^"'\\]+(\\+["'])|(\\*")(?:[^"\\]|\\[\s\S])+(")|(\$?\\*')[^']+(')`

	// Match a quoted value or one unquoted word (group 8 handles an unmatched quote). An
	// unquoted value cannot start with =, to exclude comparisons like token==null. A backslash
	// and the following character belong to the value, as in `TOKEN=a\ b`.
	secretValue = `(?:` + quotedValue + `|((?:\\*["'])?)(?:\\[\s\S]|[^\s'"=\\])(?:\\[\s\S]|[^\s'"\\])*)`

	// Match the rest of the same shell word, as in abc"def"'ghi'. Stop at shell separators or
	// quoted text containing spaces so an outer closing quote doesn't lead us to redact later
	// arguments.
	wordTail = `(?:"[^"\s]*"|'[^'\s]*'|\\[\s\S]|[^\s'";|&<>()\\])*`

	// Replace quotedValue or secretValue while preserving the key and quotes.
	keepKeyAndQuotes = "${1}${2}${4}${6}${8}" + Redacted + "${3}${5}${7}"

	// Authorization schemes followed by a single word of credentials.
	authSchemes = `bearer|basic|token|apikey|negotiate|ntlm`
)

var embeddedSecrets = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	// Keep the scheme name for Bearer, Basic, and token credentials.
	{regexp.MustCompile(`(?i)(authorization\\*["']?\s*[:=]\s*(?:\\*["'])?(?:` + authSchemes + `)\s+(?:\\*["'])?)[^\s'"]+`), "${1}" + Redacted},
	// Digest credentials can span several words, so redact up to the end of the line or a
	// single quote.
	{regexp.MustCompile(`(?i)(authorization\\*["']?\s*[:=]\s*(?:\\*["'])?(?:digest|aws4-hmac-sha256)\s+)[^'\r\n]+`), "${1}" + Redacted},
	// In scheme://user:password@host, redact up to the host even if the password contains @.
	{regexp.MustCompile(`(://[^/\s:@]+:)[^/\s]+@`), "${1}" + Redacted + "@"},
	// Assignments in shell command strings or URLs: API_KEY=value, API_KEY="value", or
	// API_KEY='value'.
	{regexp.MustCompile(`(?i)\b(` + assignmentName + `=)` + secretValue + wordTail), keepKeyAndQuotes},
	// X-Api-Key: value / "client_secret": "value"
	{regexp.MustCompile(`(?i)\b(` + headerName + `\\*["']?\s*:\s*(?:(?:` + authSchemes + `)\s+)?)` + secretValue), keepKeyAndQuotes},
	// When both key and value are quoted, redact even single-word keys like "token" or
	// 'password'.
	{regexp.MustCompile(`(?i)(\\*["'][A-Za-z0-9_.-]*(?:` + secretWords + `)\\*["']\s*:\s*)(?:` + quotedValue + `)`), keepKeyAndQuotes},
	// Handle Authorization headers not covered by the schemes above. Only redact an unquoted
	// value if nothing follows it, to avoid matching prose about authorization.
	{regexp.MustCompile(`(?i)\b(authorization\\*["']?\s*:\s*(?:[A-Za-z][A-Za-z0-9-]*\s+)?)(?:` + quotedValue + `|[^\s'"]+(\\*["']|\s*(?:\n|$)))`), "${1}${2}${4}${6}" + Redacted + "${3}${5}${7}${8}"},
}

// The value of a leading shell assignment, such as "x y" in `API_KEY="x y" cmd`.
var leadingWordPattern = regexp.MustCompile(`^` + secretValue + wordTail)

// RedactArgv returns a new slice with sensitive values masked and key names intact. It
// leaves argv unchanged. Use it before recording argv in the transcript.
//
// Argument forms:
//
//	--api-key VALUE       Mask the next argument, even if it starts with a dash.
//	--api-key=VALUE       Mask everything after the equals sign.
//	OPENAI_API_KEY=VALUE  Handle environment assignments the same way.
//
// Forms embedded within one argument (quotes are preserved):
//
//	NAME_TOKEN=value, NAME_TOKEN="value", NAME_TOKEN='value'
//	X-Api-Key: value, "client_secret": "value", "token": "value"
//	Authorization: Bearer value, Authorization: value
//	scheme://user:pass@host
//
// Redaction cannot identify every secret in arbitrary text. For example, it skips a
// space-separated `--token value` embedded in an argument and "password: value" in prose.
// Use --no-record to avoid recording at all.
func RedactArgv(argv []string) []string {
	out := make([]string, 0, len(argv))
	hideNext := false
	for _, arg := range argv {
		if hideNext {
			out = append(out, Redacted)
			hideNext = false
			continue
		}
		key, value, hasValue := strings.Cut(arg, "=")
		name := strings.TrimLeft(key, "-")
		isFlag := len(name) < len(key)
		if !keyShapePattern.MatchString(name) || !secretKeyPattern.MatchString(name) {
			out = append(out, redactEmbedded(arg))
			continue
		}
		switch {
		case hasValue && value != "" && !isFlag && strings.ContainsAny(value, " \t\n"):
			// A single argument may contain an assignment followed by a command, as in `sh -c
			// 'API_KEY=x cmd ...'`. Mask only the value so the command remains readable.
			out = append(out, key+"="+redactEmbedded(redactLeadingWord(value)))
		case hasValue && value != "":
			out = append(out, key+"="+Redacted)
		case !hasValue && isFlag:
			out = append(out, arg)
			hideNext = true
		default:
			out = append(out, arg)
		}
	}
	return out
}

// redactLeadingWord masks the first shell word in s.
func redactLeadingWord(s string) string {
	// Without the key capture, each secretValue group number decreases by one.
	return leadingWordPattern.ReplaceAllString(s, "${1}${3}${5}${7}"+Redacted+"${2}${4}${6}")
}

func redactEmbedded(arg string) string {
	for _, secret := range embeddedSecrets {
		arg = secret.pattern.ReplaceAllString(arg, secret.replacement)
	}
	return arg
}
