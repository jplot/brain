package mask_test

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/jplot/brain/internal/mask"
)

const marker = mask.Marker

const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJl"

func rep(s string, n int) string { return strings.Repeat(s, n) }

func TestMarkerValue(t *testing.T) {
	if mask.Marker != "[secret masqué]" {
		t.Fatalf("Marker = %q", mask.Marker)
	}
}

func TestMasks(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"clé ghp_" + rep("a", 36) + " fin", "clé [secret masqué] fin"},
		{"Authorization: Bearer abcdefghijklmnop1234", "Authorization: Bearer [secret masqué]"},
		{"postgres://moi:motdepasse99@hote/base", "postgres://moi:[secret masqué]@hote/base"},
		{"API_TOKEN=abcdef123456789", "API_TOKEN=[secret masqué]"},
		{`"password": "hunter2hunter2"`, `"password": "[secret masqué]"`},
		{"sk-ant-" + rep("x", 30), "[secret masqué]"},
		{"AKIA" + "A1B2C3D4E5F6G7H8", "[secret masqué]"},
		{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJl", "[secret masqué]"},
		{"Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJl", "Bearer [secret masqué]"},
		{"token=ghp_" + rep("b", 36), "token=[secret masqué]"},
		{"redis://:motdepasse99@hote:6379/0", "redis://:[secret masqué]@hote:6379/0"},
		{"postgres://moi:mot/de/passe99@hote/base", "postgres://moi:[secret masqué]@hote/base"},
		{"postgres://moi:p@ssw0rd99@hote/base", "postgres://moi:[secret masqué]@hote/base"},
		{`{"password": "Tr0ub4dor&3xyzabc"}`, `{"password": "[secret masqué]"}`},
		{`{"password": "abc,def;ghi)jkl"}`, `{"password": "[secret masqué]"}`},
		{`password: "correct horse battery"`, `password: "[secret masqué]"`},
		{`export DB_PASSWORD="correct horse battery staple"`, `export DB_PASSWORD="[secret masqué]"`},
		{"password=Pa$$w0rd,2024!xx", "password=[secret masqué]"},
		{"password=abcdefghijklmn,opqrst", "password=[secret masqué]"},
		{"password: `hunter2hunter2`", "password: `[secret masqué]`"},
		{"Le token : `abcdefghijklmnop1234`", "Le token : `[secret masqué]`"},
		{"**password**: hunter2hunter2", "**password**: [secret masqué]"},
		{"**Token** : abcdefghijklmnop1234", "**Token** : [secret masqué]"},
		{`:password => "hunter2hunter2"`, `:password => "[secret masqué]"`},
		{":api_key => 'abcdefghijklmnop1234'", ":api_key => '[secret masqué]'"},
		{`{\"password\": \"hunter2hunter2\"}`, `{\"password\": \"[secret masqué]\"}`},
		{`curl -d "{\"password\":\"hunter2hunter2\"}" https://x`, `curl -d "{\"password\":\"[secret masqué]\"}" https://x`},
		{`printf 'x\n` + "ghp_" + rep("h", 36) + `\n'`, `printf 'x\n[secret masqué]\n'`},
		{`"a\n` + jwt + `"`, `"a\n[secret masqué]"`},
		{`"a\nBearer abcdefghijklmnop1234"`, `"a\nBearer [secret masqué]"`},
		{`"\tAKIA` + "IOSFODNN7EXAMPLE" + `"`, `"\t[secret masqué]"`},
		{"token%3Dghp_" + rep("i", 36), "token%3D[secret masqué]"},
		{"x-api-key: abcdefghijklmnop1234", "x-api-key: [secret masqué]"},
		{`curl -H "X-Api-Key: abcdefghijklmnop1234"`, `curl -H "X-Api-Key: [secret masqué]"`},
		{"api-key: abcdefghijklmnop1234", "api-key: [secret masqué]"},
		{"mot_de_passe=hunter2hunter2", "mot_de_passe=[secret masqué]"},
		{"motdepasse: hunter2hunter2", "motdepasse: [secret masqué]"},
		{"DB_PASS=hunter2hunter2", "DB_PASS=[secret masqué]"},
		{"MYSQL_PWD=hunter2hunter2", "MYSQL_PWD=[secret masqué]"},
		{"passphrase: hunter2hunter2", "passphrase: [secret masqué]"},
		{"--token abcdefghijklmnop1234", "--token [secret masqué]"},
		{"--password hunter2hunter2", "--password [secret masqué]"},
		{"curl -u mon-compte:motdepasse99 https://x", "curl -u mon-compte:[secret masqué] https://x"},
		{"Authorization: Basic dXNlcjpwYXNzd29yZDEyMw==", "Authorization: Basic [secret masqué]"},
		{"Authorization: token " + rep("0123456789abcdef", 2) + "01234567", "Authorization: token [secret masqué]"},
		{"Authorization: ApiKey abcdefghijklmnop1234", "Authorization: ApiKey [secret masqué]"},
		{"ASIA" + "IOSFODNN7EXAMPLE", "[secret masqué]"},
		{"xapp-1-A0123456789-abcdefghij", "[secret masqué]"},
		{"sk_test_" + rep("a", 24), "[secret masqué]"},
		{"whsec_" + rep("a", 24), "[secret masqué]"},
		{"-----BEGIN PGP PRIVATE KEY BLOCK-----\nabc\n-----END PGP PRIVATE KEY BLOCK-----", "[secret masqué]"},
		{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0", "[secret masqué]"},
		{"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0B\n", "-----BEGIN PRIVATE KEY-----\n[secret masqué]\n"},
		{"DB_SECRET_URL=redis://u:motdepasse99@h:6379", "DB_SECRET_URL=redis://u:[secret masqué]@h:6379"},
		{"token=abcde-ghp_" + rep("j", 36), "token=abcde-[secret masqué]"},
		{"ghp_" + rep("Q", 30) + "ghp_" + rep("R", 30), "[secret masqué]"},
		{"AKIA" + rep("Q", 16) + "ghp_" + rep("R", 36), "[secret masqué]"},
		{"token=abc123.def456", "token=[secret masqué]"},
		{"token=abc123def456.ghi789jkl012", "token=[secret masqué]"},
		{"password=Hunter2.Hunter2xx", "password=[secret masqué]"},
		{"token=eyJhbGciOiJIUzI1NiJ9.abc", "token=[secret masqué]"},
		{"api_token=" + rep("Q", 14) + ".Bearer " + rep("Q", 20), "api_token=[secret masqué] [secret masqué]"},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			if got := mask.Text(c.raw); got != c.want {
				t.Errorf("Text(%q)\n got %q\nwant %q", c.raw, got, c.want)
			}
		})
	}
}

func TestLeavesAlone(t *testing.T) {
	cases := []string{
		"le token est expiré", "password: court", "commit 3a51530", "sk-learn est une bibliothèque",
		"https://github.com/mon-compte/depot", "secret_key_base est une variable Rails",
		"const token = localStorage.getItem('token');",
		"token: request.headers['Authorization']",
		"password: params[:password]",
		"api_key = os.environ.get('API_KEY')",
		"token = generate_token()",
		"secret = Rails.application.credentials.secret_key_base",
		"self.password_confirmation = password_confirmation",
		"password: user.encrypted_password,",
		"token_uri: https://oauth2.googleapis.com/token",
		"TOKEN_URL=https://auth.example.com/oauth/token",
		"secretName: tls-certificate-prod",
		`"total_tokens": 123456789012`,
		"API_TOKEN=$API_TOKEN_FROM_ENV",
		"Le token: authentification",
		"Le secret : confidentialité",
		"password: réinitialisation",
		"the Bearer authentication-scheme is used",
		"sk-learn-contrib-imbalanced-learn est une lib",
		"Le secret :\n\nAuthentification réussie ce matin",
		"password=motdepassefacile",
		"secret=ab.cd.ef.gh.ij.kl.mn",
		"tokenize='unicode61 remove_diacritics 2'",
		"tokenizer: AutoTokenizer.from_pretrained",
		"cherche `-----BEGIN PRIVATE KEY-----` dans les fichiers, puis on passe à la suite.\nDeuxième paragraphe.",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if got := mask.Text(c); got != c {
				t.Errorf("Text(%q) = %q", c, got)
			}
		})
	}
}

func TestWholePEMBlock(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\nabcdef\n-----END RSA PRIVATE KEY-----"
	if got := mask.Text("avant\n" + pem + "\naprès"); got != "avant\n[secret masqué]\naprès" {
		t.Errorf("got %q", got)
	}
}

func TestIdempotent(t *testing.T) {
	cases := []string{
		"Bearer abcdefghijklmnop1234 et token=ghp_" + rep("c", 36),
		"postgres://moi:motdepasse99@hote/base",
		"-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----",
		`"password": "hunter2hunter2"`,
	}
	for _, c := range cases {
		once := mask.Text(c)
		if again := mask.Text(once); again != once {
			t.Errorf("not idempotent: %q then %q", once, again)
		}
		if strings.Contains(once, marker+marker) {
			t.Errorf("doubled marker in %q", once)
		}
	}
}

func TestAccentedLettersAreWordCharacters(t *testing.T) {
	token := synthetic(181, 1, alnum)
	cases := []struct{ raw, want string }{
		{"é" + token, "é" + token},
		{"éàtoken=abcdef123456789", "éàtoken=" + marker},
		{"réseau_token=abcdef123456789", "réseau_token=" + marker},
		{"mot de passe: \"abcdefghijklmnop1234\"", "mot de passe: \"abcdefghijklmnop1234\""},
	}
	for _, c := range cases {
		if got := mask.Text(c.raw); got != c.want {
			t.Errorf("Text(%q)\n got %q\nwant %q", c.raw, got, c.want)
		}
	}
}

func TestUnicodeClassesFollowPython(t *testing.T) {
	nfdToken := "e\u0301A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0Uv"
	cases := []struct{ raw, want string }{
		{nfdToken, "e\u0301" + marker},
		{"pa\u017f\u017fword=hunter2hunter2", "pa\u017f\u017fword=" + marker},
		{"PA\u017f\u017fWORD=hunter2hunter2", "PA\u017f\u017fWORD=" + marker},
		{"\u00b2token=abcdef123456789", "\u00b2token=" + marker},
		{"\u00bctoken=abcdef123456789", "\u00bctoken=" + marker},
		{"\u0663token=abcdef123456789", "\u0663token=" + marker},
		{"\u0301token=abcdef123456789", "\u0301token=" + marker},
		{"Authorization: Ba\u017fic dXNlcjpwYXNzd29yZDEyMw==", "Authorization: Ba\u017fic " + marker},
		{"curl\u0301 -u a:b12345678", "curl\u0301 -u a:" + marker},
		{"\u00b2A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0Uv", "\u00b2A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0Uv"},
		{"token\u203fx=abcdef123456789", "token\u203fx=abcdef123456789"},
		{"token=abc\x1cdef123456789xyz", "token=abc\x1cdef123456789xyz"},
		{"tok\u212aen=abcdef123456789", "tok\u212aen=abcdef123456789"},
		{"pass\u017f=abcdef123456789", "pass\u017f=abcdef123456789"},
	}
	for _, c := range cases {
		if got := mask.Text(c.raw); got != c.want {
			t.Errorf("Text(%+q)\n got %+q\nwant %+q", c.raw, got, c.want)
		}
	}
}

func TestPathologicalInputReturnsQuickly(t *testing.T) {
	inputs := map[string]string{
		"a=":           rep("a=", 200000),
		"A":            rep("A", 1<<20),
		"eyJ":          "eyJ" + rep("a.", 100000),
		"a.":           rep("a.", 20000),
		"base64url":    rep("abcDEF0123456789_-", 6000),
		"uuid":         strings.Join(repSlice("123e4567-e89b-12d3-a456-426614174000", 3000), "-"),
		"token":        rep("token", 20000),
		"a=b;":         rep("a=b;", 25000),
		"a;":           rep("a;", 50000),
		"a=bbbb;":      rep("a="+rep("b", 4000)+";", 25),
		"64 a =":       rep(rep("a", 64)+"=", 1500),
		"x=123;":       rep("x=1234567890123456789; ", 5000),
		"n=b1":         rep("n="+rep("b1", 2500)+";", 20),
		"access_token": rep("access_token\n: \n", 6000),
		"newlines":     rep("\n", 100000),
		"space nl":     rep(" \n", 50000),
		"A1b":          rep("A1b", 33000),
		"Aa1Bb2":       rep("Aa1Bb2Cc3Dd4-", 8000),
	}
	for name, in := range inputs {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			out := mask.Text(in)
			if d := time.Since(start); d >= 2*time.Second {
				t.Fatalf("took %v", d)
			}
			if out == marker {
				t.Errorf("deadline fired: whole input was replaced by the marker")
			}
			if again := mask.Text(out); again != out {
				t.Errorf("not idempotent")
			}
		})
	}
}

func repSlice(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestDeepNestingDoesNotBlowUp(t *testing.T) {
	for _, motif := range []string{"token=a(", "a.token=b(c);"} {
		for _, n := range []int{997, 5000, 50000} {
			t.Run(fmt.Sprintf("%s x%d", motif, n), func(t *testing.T) {
				start := time.Now()
				out := mask.Text(rep(motif, n))
				if d := time.Since(start); d >= 5*time.Second {
					t.Fatalf("took %v", d)
				}
				if out == marker {
					t.Errorf("deadline fired: whole input was replaced by the marker")
				}
				if again := mask.Text(out); again != out {
					t.Errorf("not idempotent")
				}
			})
		}
	}
}

var fragments = map[string]string{
	"gh": "ghp_" + rep("Q", 36), "pat": "github_pat_" + rep("Q", 30), "ant": "sk-ant-" + rep("Q", 30),
	"sk": "sk-" + rep("Q", 30), "hf": "hf_" + rep("Q", 30), "akia": "AKIA" + rep("Q", 16),
	"xox": "xoxb-" + rep("Q", 20), "glpat": "glpat-" + rep("Q", 20),
	"sg": "SG." + rep("Q", 20) + "." + rep("Q", 20), "live": "sk_live_" + rep("Q", 20),
	"jwt":    "eyJ" + rep("Q", 10) + ".eyJ" + rep("Q", 10) + "." + rep("Q", 10),
	"bearer": "Bearer " + rep("Q", 20),
	"pem":    "-----BEGIN PRIVATE KEY-----\n" + rep("Q", 20) + "\n-----END PRIVATE KEY-----",
	"url":    "postgres://moi:" + rep("Q", 14) + "@hote/base",
	"kv":     "api_token=" + rep("Q", 14), "kvq": `"password": "` + rep("Q", 14) + `"`,
}

func TestGluedFragmentsLeaveNoDebrisNorDoubleMarker(t *testing.T) {
	prefixes := []string{"", "x", "abcde", "abcde-", "token=", "token=abcde-", "password: ", "secret=abcdef/",
		`password="abcde`, "Bearer ", "Authorization: Bearer ", "://u:", "https://u:p"}
	joins := []string{"", " ", "=", ":", ": ", "-", ".", "/", ",", "&", "@", "\n", "_"}
	names := make([]string, 0, len(fragments))
	for n := range fragments {
		names = append(names, n)
	}
	sortStrings(names)
	rng := rand.New(rand.NewPCG(20261103, 1))
	pick := func(s []string) string { return s[rng.IntN(len(s))] }
	for range 4000 {
		text := pick(prefixes) + fragments[pick(names)] + pick(joins) + fragments[pick(names)]
		masked := mask.Text(text)
		if again := mask.Text(masked); again != masked {
			t.Fatalf("not idempotent: %q", text)
		}
		if strings.Contains(masked, marker+marker) {
			t.Fatalf("double marker: %q", text)
		}
		if strings.Contains(strings.ReplaceAll(masked, marker, ""), "masqué]") {
			t.Fatalf("debris: %q -> %q", text, masked)
		}
		if strings.Contains(masked, "QQQQ") {
			t.Fatalf("leak: %q -> %q", text, masked)
		}
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func synthetic(length int, seed uint64, alphabet string) string {
	rng := rand.New(rand.NewPCG(seed, 7))
	var b strings.Builder
	for range length {
		b.WriteByte(alphabet[rng.IntN(len(alphabet))])
	}
	return b.String()
}

const cookieAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

var (
	token181    = synthetic(181, 1, alnum)
	key46       = "aaa_" + synthetic(42, 2, alnum)
	cookieNames = []string{"PHPSESSID", "cf_clearance", "__cf_bm", "_cfuvid", "visid_incap_2207353", "incap_ses_985_2207353"}
	cookies     = map[string]string{
		"PHPSESSID":             synthetic(26, 3, cookieAlphabet),
		"cf_clearance":          synthetic(300, 4, cookieAlphabet+"-."),
		"__cf_bm":               synthetic(120, 5, cookieAlphabet+"-_."),
		"_cfuvid":               synthetic(70, 6, cookieAlphabet+"-_."),
		"visid_incap_2207353":   synthetic(80, 7, alnum),
		"incap_ses_985_2207353": synthetic(60, 8, alnum),
	}
)

const cookieTail = "tarteaucitron=!x=true!y=false; utm_term=abc"

func cookieLine(masked bool) string {
	var parts []string
	for _, n := range cookieNames {
		if masked {
			parts = append(parts, n+"="+marker)
		} else {
			parts = append(parts, n+"="+cookies[n])
		}
	}
	return strings.Join(parts, "; ") + "; " + cookieTail
}

func TestRealWorldShapesMasked(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"access_token\n: \n\"" + token181 + "\"\nexpires_at\n: \n\"2026-11-03T10:00:00Z\"",
			"access_token\n: \n\"" + marker + "\"\nexpires_at\n: \n\"2026-11-03T10:00:00Z\""},
		{"access_token\n: \n\"" + token181[:30] + "\"", "access_token\n: \n\"" + marker + "\""},
		{"access_token\n: \n" + token181[:30] + "\nfin", "access_token\n: \n" + marker + "\nfin"},
		{"access_token\n:" + token181[:30], "access_token\n:" + marker},
		{"access_token\r\n: \r\n" + token181[:30], "access_token\r\n: \r\n" + marker},
		{"API Key : " + key46, "API Key : " + marker},
		{"API Key : " + key46[:30], "API Key : " + marker},
		{"api key: " + key46[:30], "api key: " + marker},
		{"x-api key=" + key46[:30], "x-api key=" + marker},
		{cookieLine(false), cookieLine(true)},
		{"Cookie: a1=" + rep("x", 10) + "12345678; b2=short", "Cookie: a1=" + marker + "; b2=short"},
		{"transfert.sh " + token181, "transfert.sh " + marker},
		{"voici " + token181 + ".", "voici " + marker + "."},
		{"https://auth.example.com/cb?state=abc&code=" + synthetic(200, 9, alnum) + "&scope=x",
			"https://auth.example.com/cb?state=abc&code=" + marker + "&scope=x"},
		{"https://auth.example.com/cb?code=" + synthetic(200, 10, alnum+"-_"),
			"https://auth.example.com/cb?code=" + marker},
	}
	for _, c := range cases {
		if got := mask.Text(c.raw); got != c.want {
			t.Errorf("Text(%.60q)\n got %.200q\nwant %.200q", c.raw, got, c.want)
		}
	}
}

func TestRealWorldShapesNotMasked(t *testing.T) {
	cases := []string{
		"token:\n  hunter2hunter2",
		"token:\n\n  hunter2hunter2",
		"Le secret\n: confidentialité\nsuite",
		"API Key : your key here",
		"API key is required",
		"PHPSESSID=" + cookies["PHPSESSID"],
		"export PATH=/usr/local/bin/foo; FOO=bar",
		"color:red;margin=0",
		"A=./build/out1234567890123; B=~/dir1234567890123456; C=$HOME1234567890123456; D=x",
		"a=/1234567890123456x; b=abc",
		"utm_term=abc; utm_source=newsletter2026",
		cookieTail,
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4",
		"git checkout e3b0c44298fc1c149afbf4c8996fb92427ae41e4 puis ça",
		"4f1c2a7e-9b3d-4e5a-8c6f-1d2e3f4a5b6c",
		"4f1c2a7e-9b3d-4e5a-8c6f-1d2e3f4a5b6c-4f1c2a7e-9b3d-4e5a-8c6f-1d2e3f4a5b6c",
		"mise-a-jour-des-resumes-apres-oubli-pendant-lappel-perime",
		"test_resume_perime_oubli_pendant_l_appel_reste_perime_Version2_Final_3_4_5",
		"/Users/x/Projects/worktrees/4f1c2a7e-9b3d-4e5a-8c6f-1d2e3f4a5b6c/src/Main_2026_11_03.py",
		"A000_Rapport_Mensuel_FooBar_ABC_2026-10-26_12_00_00_UTC.csv",
		"/Users/x/" + token181 + "/fichier",
		token181 + ".json",
		token181[:39],
		rep("Aa1", 12),
		marker,
		"transfert.sh " + marker,
		"a1=" + marker + "; b2=" + marker,
	}
	for _, c := range cases {
		if got := mask.Text(c); got != c {
			t.Errorf("Text(%.80q) = %.200q", c, got)
		}
	}
}

func TestRealWorldShapesIdempotent(t *testing.T) {
	cases := []string{
		"access_token\n: \n\"" + token181 + "\"",
		"API Key : " + key46,
		cookieLine(false),
		"transfert.sh " + token181,
		marker + " " + cookieLine(false),
		"PHPSESSID=" + marker + "; cf_clearance=" + cookies["cf_clearance"],
		"transfert.sh " + token181 + " " + marker,
		marker + token181,
		"API Key : " + marker,
		"access_token\n: \n\"" + marker + "\"",
	}
	for _, c := range cases {
		once := mask.Text(c)
		if again := mask.Text(once); again != once {
			t.Errorf("not idempotent: %.80q", c)
		}
		if strings.Contains(once, marker+marker) {
			t.Errorf("doubled marker in %.200q", once)
		}
		if strings.Contains(once, token181) || strings.Contains(once, cookies["cf_clearance"]) {
			t.Errorf("leak in %.200q", once)
		}
	}
}

func TestCookieNextToMarkerIsMasked(t *testing.T) {
	got := mask.Text("PHPSESSID=" + marker + "; cf_clearance=" + cookies["cf_clearance"])
	if want := "PHPSESSID=" + marker + "; cf_clearance=" + marker; got != want {
		t.Errorf("got %q", got)
	}
}
