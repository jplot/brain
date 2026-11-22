// Package mask hides secrets in text before it reaches the database.
package mask

import (
	"math"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2"
)

const Marker = "[secret masqué]"

// budget bounds one Text call. Every expression gets the time left as its match
// timeout; whatever is not examined in time becomes Marker (text is lost, never leaked).
const budget = time.Second

// maxDepth: 997 consecutive "token=a(" (7,976 characters) overflowed the Python stack.
const maxDepth = 8

var (
	mu      sync.Mutex
	marker  = regexp2.Escape(Marker)
	quote   = `\\?["'` + "`]"
	token   = `(?:(?![Bb]earer[ \t])[A-Za-z0-9_-])`
	authVal = `(?:(?![Bb]earer[ \t])[A-Za-z0-9._~+/=-])`
	bare    = `(?:` + marker + "|[^\\s\"'`])+"

	prefixed = `gh[pousr]_` + token + `{20,}|github_pat_` + token + `{20,}|sk-ant-` + token + `{20,}` +
		`|sk-(?=[A-Za-z0-9_-]*[0-9A-Z])` + token + `{20,}|hf_` + token + `{20,}` +
		`|A[KS]IA[0-9A-Z]{16}|xox[abprs]-` + token + `{10,}|xapp-` + token + `{10,}` +
		`|glpat-` + token + `{20,}|SG\.` + token + `{16,}\.` + token + `{16,}` +
		`|[sr]k_(?:live|test)_` + token + `{16,}|whsec_` + token + `{16,}`
	jwt        = `eyJ` + token + `{5,}\.(?:` + token + `{5,}\.` + token + `*|eyJ` + token + `{5,})`
	privateKey = `-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----`
	endKey     = `-----END [A-Z ]*PRIVATE KEY(?: BLOCK)?-----`
	kebabWord  = `(?![a-zà-ÿ]+(?:-[a-zà-ÿ]+)+(?![\w-]))`

	// Names are bounded to 64 characters on each side and anchored on a name start:
	// 100 KB of base64url or repeated "a." took over 17 s with a bare quantifier.
	name = `(?<![\w.-])[\w.-]{0,64}(?:(?:password|passwd|passphrase|secret|token|api[_ -]?key` +
		`|mot_?de_?passe)(?-i:(?![a-z]))|(?<![A-Za-z])(?:pass|pwd)(?![A-Za-z]))[\w.-]{0,64}`

	cookiePair = `(?<![\w.=-])[\w.-]{1,64}=(?:` + marker + `|[^\s;]){1,4096}`
)

var (
	keyed = mustCompile(`(?<head>`+name+`(?:`+quote+`|\*{1,2})?`+
		`(?:[ \t]*\r?\n[ \t]*:[ \t]*(?:\r?\n[ \t]*)?|[ \t]*(?:=>|[:=])[ \t]*))`+
		`(?:(?<q>`+quote+`)(?:(?!\k<q>)(?:\\.|[^\n])){12,500}\k<q>`+
		`|(?<bare>`+bare+`))`, regexp2.IgnoreCase)
	flag = mustCompile(`(?<head>(?<![\w-])--(?:token|password|passwd|passphrase|secret|api-?key)[ \t]+)`+
		`(?<bare>`+bare+`)`, 0)
	markerValue = mustCompile(`(?<head>`+marker+`[=:][ \t]*)(?<bare>`+bare+`)`, 0)
	cookies     = mustCompile(cookiePair+`(?:;[ \t]*`+cookiePair+`)+`, 0)
	cookieValue = mustCompile(`(?<head>(?<![\w.=-])[\w.-]{1,64}=)(?<value>(?:`+marker+`|[^\s;])+)`, 0)
	bareToken   = mustCompile(`(?<![\w/-])(?<!\w\.)[A-Za-z0-9_-]{40,}(?![\w/-]|\.\w)`, 0)
	dottedName  = mustCompile(`\A(?:[a-z_]+|[A-Z][a-z_]*)(?:\.(?:[a-z_]+|[A-Z][a-z_]*))+\z`, 0)
	scheme      = mustCompile(`\A[a-z][a-z0-9+.-]{1,12}://`, 0)
	doubled     = mustCompile(`(?:`+marker+`(?:\.`+token+`{8,})*){2,}|`+marker+`(?:\.`+token+`{8,})+`, 0)
	pemBlock    = mustCompile(privateKey+`.*?`+endKey, regexp2.Singleline)
	pemBody     = mustCompile(`(?<h>`+privateKey+`\s*)[A-Za-z0-9+/=]{16,}`+
		`(?:[ \t]*\r?\n[A-Za-z0-9+/=]+(?=[ \t]*(?:\r?\n|\z)))*`, 0)
	jwtRe       = mustCompile(jwt, 0)
	prefixedRe  = mustCompile(prefixed, 0)
	urlPassword = mustCompile(`(?<h>://[^\s:/@]*:)(?!\d{1,5}[/?#])[^\s]+(?=@)`, 0)
	authHeader  = mustCompile(`(?<h>\bAuthorization["']?[ \t]*[:=][ \t]*["']?(?:Basic|token|Api-?Key)[ \t]+)`+
		authVal+`{8,}`, regexp2.IgnoreCase)
	bearer       = mustCompile(`(?<h>[Bb]earer[ \t]+)`+kebabWord+authVal+`{16,}`, 0)
	curlPassword = mustCompile(`(?<h>\bcurl\b[^\n]{0,300}?[ \t]-u[ \t]+["']?[^\s:"']+:)`+
		`(?:`+marker+`|[^\s"'])+`, 0)
)

func mustCompile(pattern string, opts regexp2.RegexOptions) *regexp2.Regexp {
	return regexp2.MustCompile(pattern, opts)
}

func init() { regexp2.SetTimeoutCheckPeriod(5 * time.Millisecond) }

type run struct{ deadline time.Time }

type step struct {
	re *regexp2.Regexp
	fn func(*run, *regexp2.Match) string
}

func toMarker(*run, *regexp2.Match) string { return Marker }

func keepHead(_ *run, m *regexp2.Match) string { return group(m, "h") + Marker }

func headIfSecret(_ *run, m *regexp2.Match) string {
	if isSecretValue(group(m, "bare")) {
		return group(m, "head") + Marker
	}
	return m.String()
}

var steps = []step{
	{pemBlock, toMarker},
	{pemBody, keepHead},
	{jwtRe, toMarker},
	{prefixedRe, toMarker},
	{urlPassword, keepHead},
	{authHeader, keepHead},
	{bearer, keepHead},
	{keyed, func(r *run, m *regexp2.Match) string { return r.maskKeyed(m, 0) }},
	{flag, headIfSecret},
	{markerValue, headIfSecret},
	{cookies, func(r *run, m *regexp2.Match) string { return r.maskCookies(m) }},
	{curlPassword, keepHead},
	{bareToken, func(_ *run, m *regexp2.Match) string { return maskBareToken(m.String()) }},
}

func group(m *regexp2.Match, name string) string { return m.GroupByName(name).String() }

// sub replaces every match of re in text with fn's result. Past the deadline, or when
// re times out, the unexamined rest is replaced by Marker.
func (r *run) sub(re *regexp2.Regexp, text string, fn func(*regexp2.Match) string) string {
	runes := []rune(text)
	var out strings.Builder
	last := 0
	var m *regexp2.Match
	for {
		left := time.Until(r.deadline)
		var err error
		if left > 0 {
			re.MatchTimeout = left
			if m == nil {
				m, err = re.FindRunesMatch(runes)
			} else {
				m, err = re.FindNextMatch(m)
			}
		}
		if left <= 0 || err != nil {
			out.WriteString(Marker)
			return out.String()
		}
		if m == nil {
			out.WriteString(string(runes[last:]))
			return out.String()
		}
		out.WriteString(string(runes[last:m.Index]))
		out.WriteString(fn(m))
		last = m.Index + m.Length
	}
}

func (r *run) maskKeyed(m *regexp2.Match, depth int) string {
	head := group(m, "head")
	value := group(m, "bare")
	if value == "" {
		q := group(m, "q")
		return head + q + Marker + q
	}
	if isSecretValue(value) || depth >= maxDepth {
		return head + Marker
	}
	return head + r.sub(keyed, value, func(n *regexp2.Match) string { return r.maskKeyed(n, depth+1) })
}

func (r *run) maskCookies(m *regexp2.Match) string {
	return r.sub(cookieValue, m.String(), func(n *regexp2.Match) string {
		if isCookieSecret(group(n, "value")) {
			return group(n, "head") + Marker
		}
		return n.String()
	})
}

func matches(re *regexp2.Regexp, s string) bool {
	ok, err := re.MatchString(s)
	return err == nil && ok
}

func isSecretValue(value string) bool {
	core := strings.TrimRight(strings.ReplaceAll(value, Marker, ""), ",;.")
	if utf8.RuneCountInString(core) < 12 || strings.ContainsAny(core, "([") || strings.HasPrefix(core, "$") {
		return false
	}
	if matches(scheme, core) || matches(dottedName, core) {
		return false
	}
	words := strings.NewReplacer("_", "", "-", "").Replace(core)
	return !(isAlpha(words) && core == strings.ToLower(core))
}

func isAlpha(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !unicode.IsLetter(c) {
			return false
		}
	}
	return true
}

func isCookieSecret(value string) bool {
	return utf8.RuneCountInString(value) >= 16 && strings.IndexFunc(value, unicode.IsDigit) >= 0 &&
		!hasAnyPrefix(value, "/", "./", "~", "$") && isSecretValue(value)
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func entropy(run string) float64 {
	counts := map[rune]int{}
	n := 0
	for _, c := range run {
		counts[c]++
		n++
	}
	var h float64
	for _, k := range counts {
		p := float64(k) / float64(n)
		h -= p * math.Log2(p)
	}
	return h
}

func maskBareToken(run string) string {
	var digits, upper, lower int
	for _, c := range run {
		switch {
		case unicode.IsDigit(c):
			digits++
		case unicode.IsUpper(c):
			upper++
		case unicode.IsLower(c):
			lower++
		}
	}
	if digits >= 4 && upper >= 3 && lower >= 3 && entropy(run) >= 4.5 {
		return Marker
	}
	return run
}

func (r *run) once(text string) string {
	for _, s := range steps {
		text = r.sub(s.re, text, func(m *regexp2.Match) string { return s.fn(r, m) })
	}
	return r.sub(doubled, text, func(*regexp2.Match) string { return Marker })
}

// Text masks every secret in s. It is idempotent: a pattern can reveal another once the
// first is masked ("AKIA…ghp_…"), so passes repeat until the text stops changing.
func Text(s string) string {
	mu.Lock()
	defer mu.Unlock()
	r := &run{deadline: time.Now().Add(budget)}
	for {
		masked := r.once(s)
		if masked == s {
			return s
		}
		s = masked
	}
}
