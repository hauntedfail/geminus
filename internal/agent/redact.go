package agent

import (
	"regexp"
	"strings"
)

// Redacted は伏せた値の代わりに入れる文字列。
const Redacted = "[REDACTED]"

// 値を伏せるキー名。--api-key / --token / --password 系で、--accessToken のような
// 連結形も拾う。末尾一致なので --max-tokens や --tokenizer のような別の語は対象にしない。
// pwd は区切りの後ろだけ（MYSQL_PWD）、pass は区切りの後ろか単独（DB_PASS、--pass）を取る。
// PWD や --bypass は対象にしない。
var secretKeyPattern = regexp.MustCompile(`(?i)(` + secretKeyNames + `|token|password|passwd|passphrase|secret|credentials?|auth|authorization|[-_]pwd|(?:^|[-_])pass)$`)

// フラグ名や環境変数名として読める形。プロンプト本文中の "=" を誤って拾わないための条件。
var keyShapePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// 以下は1つの引数の中に埋まった秘密を探すための部品。cmd バックエンドの `sh -c '...'` や
// curl のヘッダで現れる。引数は文章（プロンプト）のこともあるので、フラグ名より狭く取る。
const (
	// api-key / access_key / SECRET_ACCESS_KEY / private-key
	secretKeyNames = `(?:api|access|secret|private)[-_]?key`

	// 単独でも秘密を指す語。
	secretWords = secretKeyNames + `|token|secret|password|passwd|passphrase`

	// 代入 NAME=value の NAME。auth と credentials は入れない
	// （oauth=1 や credentials: 'include' のように、文章やコードに普通に現れる）。
	// pass と pwd は区切りの後ろだけ（"pass=3 fail=2" を巻き込まない）。
	assignmentName = `[A-Za-z0-9_]*(?:` + secretWords + `|_pwd|_pass)`

	// ヘッダ Name: value の Name。区切りを含む複合語だけを取る
	// （X-Api-Key、X-Auth-Token、client_secret）。"Unexpected token: }" や "password: ..." の
	// ような文章を巻き込まないよう、token / secret / password 単独は対象にしない。
	headerName = `(?:[A-Za-z0-9_-]*` + secretKeyNames + `|[A-Za-z0-9_-]*[-_](?:token|secret|password|passwd|passphrase|authorization))`

	// 引用符で囲んだ値。左から順に \"...\"、"..."、'...'（$'...' を含む）。
	// 1つ目は `ssh host "API_KEY=\"x y\" cmd"` のように、引用符ごとエスケープされた形。
	// キャプチャは 2〜7 番を使う（開く側が偶数、閉じる側が奇数）。1 番はキー側のもの。
	quotedValue = `(\\+["'])[^"'\\]+(\\+["'])|(\\*")(?:[^"\\]|\\[\s\S])+(")|(\$?\\*')[^']+(')`

	// 値。引用符で囲んだ形か、引用符なしの1語（8 番は閉じていない引用符）。
	// 引用符なしの値は "=" で始まらない（token==null のような比較を巻き込まない）。
	// バックスラッシュは次の1文字と組で値に含める（`TOKEN=a\ b`）。
	secretValue = `(?:` + quotedValue + `|((?:\\*["'])?)(?:\\[\s\S]|[^\s'"=\\])(?:\\[\s\S]|[^\s'"\\])*)`

	// シェルの1語として値に続く部分（abc"def"'ghi'）。空白を含む引用は続きと見なさない。
	// 外側の引用符の中にいる場合に、閉じ引用符の先の引数まで伏せないため。
	// シェルの区切り（; | & など）でも止める。
	wordTail = `(?:"[^"\s]*"|'[^'\s]*'|\\[\s\S]|[^\s'";|&<>()\\])*`

	// quotedValue / secretValue を伏せ、キーと引用符を残す置換。
	keepKeyAndQuotes = "${1}${2}${4}${6}${8}" + Redacted + "${3}${5}${7}"

	// Authorization の方式名のうち、後ろに資格情報が1語で続くもの。
	authSchemes = `bearer|basic|token|apikey|negotiate|ntlm`
)

var embeddedSecrets = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	// Authorization: Bearer <token> / Basic <base64> / token <token>。方式名は残す。
	{regexp.MustCompile(`(?i)(authorization\\*["']?\s*[:=]\s*(?:\\*["'])?(?:` + authSchemes + `)\s+(?:\\*["'])?)[^\s'"]+`), "${1}" + Redacted},
	// Authorization: Digest username="u", response="..."。資格情報が複数の語にまたがるので、
	// 行末か単一引用符まで伏せる。
	{regexp.MustCompile(`(?i)(authorization\\*["']?\s*[:=]\s*(?:\\*["'])?(?:digest|aws4-hmac-sha256)\s+)[^'\r\n]+`), "${1}" + Redacted},
	// scheme://user:password@host。パスワードに "@" が入っていてもホストの直前まで伏せる。
	{regexp.MustCompile(`(://[^/\s:@]+:)[^/\s]+@`), "${1}" + Redacted + "@"},
	// API_KEY=value / API_KEY="value" / API_KEY='value'（シェルのコマンド文字列や URL の中）
	{regexp.MustCompile(`(?i)\b(` + assignmentName + `=)` + secretValue + wordTail), keepKeyAndQuotes},
	// X-Api-Key: value / "client_secret": "value"
	{regexp.MustCompile(`(?i)\b(` + headerName + `\\*["']?\s*:\s*(?:(?:` + authSchemes + `)\s+)?)` + secretValue), keepKeyAndQuotes},
	// "token": "value" / 'password': 'value'。キーも値も引用符付きなら、単独の語でも伏せる。
	{regexp.MustCompile(`(?i)(\\*["'][A-Za-z0-9_.-]*(?:` + secretWords + `)\\*["']\s*:\s*)(?:` + quotedValue + `)`), keepKeyAndQuotes},
	// 上の方式名に当たらない Authorization: value / Authorization: Scheme value。
	// 引用符なしの値は、そこで終わっている場合だけ伏せる
	// （"authorization: handled by the middleware" のような文章を巻き込まない）。
	{regexp.MustCompile(`(?i)\b(authorization\\*["']?\s*:\s*(?:[A-Za-z][A-Za-z0-9-]*\s+)?)(?:` + quotedValue + `|[^\s'"]+(\\*["']|\s*(?:\n|$)))`), "${1}${2}${4}${6}" + Redacted + "${3}${5}${7}${8}"},
}

// シェルのコマンド文字列の先頭にある代入の値（`API_KEY="x y" cmd` の `"x y"`）。
var leadingWordPattern = regexp.MustCompile(`^` + secretValue + wordTail)

// RedactArgv は argv のうち秘密を運ぶ値だけを伏せた新しいスライスを返す。
// キー名は残す。argv 自体は変更しない。transcript に argv を残す前に通す。
//
// 伏せる形:
//
//	--api-key VALUE       次の引数を伏せる（VALUE が "-" で始まっていても伏せる）
//	--api-key=VALUE       "=" の後ろを伏せる
//	OPENAI_API_KEY=VALUE  env 形式も同様
//
// 1つの引数の中に埋まった形（引用符は残す）:
//
//	NAME_TOKEN=value、NAME_TOKEN="value"、NAME_TOKEN='value'
//	X-Api-Key: value、"client_secret": "value"、"token": "value"
//	Authorization: Bearer value、Authorization: value
//	scheme://user:pass@host
//
// 取りこぼしはあり得る（任意の文字列から秘密を見分ける方法は無い）。たとえば
// 引数の中の `--token value`（空白区切り）や、文章の中の "password: value" は伏せない。
// 記録そのものを避けたい場合は --no-record を使う。
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
			// `sh -c 'API_KEY=x cmd ...'` のように、代入の後ろにコマンドが続く1引数。
			// 値の部分だけを伏せ、残りは読めるままにする。
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

// redactLeadingWord は s の先頭にあるシェルの1語を伏せる。
func redactLeadingWord(s string) string {
	// キー側のキャプチャが無いので、secretValue の番号は 1 つずつ前にずれる。
	return leadingWordPattern.ReplaceAllString(s, "${1}${3}${5}${7}"+Redacted+"${2}${4}${6}")
}

func redactEmbedded(arg string) string {
	for _, secret := range embeddedSecrets {
		arg = secret.pattern.ReplaceAllString(arg, secret.replacement)
	}
	return arg
}
