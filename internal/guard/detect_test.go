package guard

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// Every secret below is invented, and assembled from pieces at run time so
// that no credential-shaped string appears in the repository for a
// scanner to trip over. j joins the pieces.
func j(parts ...string) string { return strings.Join(parts, "") }

// rnd is a fixed string of mixed characters to cut secrets from.
const rnd = "q7Rz2Lp9XvB4nKd8Yt3WmHs6JcF5gPa1ZeUo0QiNxTbVrMlCkEyDwGhSuAfIjO7q2Rz9Lp4XvB8nKd3Yt6WmHs5JcF1gPa0ZeUoQiNxTb"

type positive struct {
	kind string
	text string // the text to scan
	want string // the secret that should be found in it
}

func positives() []positive {
	anthropic := j("sk-", "ant-", "api03-", rnd[:40])
	openai := j("sk-", "proj-", rnd[5:53])
	awsID := j("AKIA", "Q7RZ2LP9XVB4NKD8")
	awsSecret := j(rnd[10:30], "/", rnd[40:59])
	github := j("gh", "p_", rnd[:36])
	githubPAT := j("github", "_pat_", rnd[:22], "_", rnd[30:89])
	slack := j("xox", "b-", "204857361942-", rnd[:24])
	google := j("AI", "za", rnd[:35])
	stripe := j("sk", "_live_", rnd[:28])
	jwt := j("ey", "J", rnd[:20], ".", "ey", "J", rnd[20:60], ".", rnd[60:96])
	pem := j("-----BEGIN ", "RSA PRIVATE KEY-----\n", rnd[:64], "\n", rnd[30:94], "\n-----END ", "RSA PRIVATE KEY-----")
	dbPass := j("Tr0ub4dor", "-x9Kq")
	generic := j("f9Kx", "2mQv", "8LpR", "4tZw")

	return []positive{
		{"anthropic_api_key", "export ANTHROPIC_API_KEY=" + anthropic, anthropic},
		{"anthropic_api_key", `headers = {"x-api-key": "` + anthropic + `"}`, anthropic},
		{"openai_api_key", "OPENAI_API_KEY=" + openai + "\n", openai},
		{"aws_access_key", "aws_access_key_id = " + awsID, awsID},
		{"aws_access_key", "found key " + awsID + " in the logs", awsID},
		{"aws_secret_key", "aws_secret_access_key = " + awsSecret, awsSecret},
		{"aws_secret_key", `"AWS_SECRET_ACCESS_KEY": "` + awsSecret + `"`, awsSecret},
		{"github_token", "git clone https://" + github + "@github.com/acme/app.git", github},
		{"github_token", "GH_TOKEN: " + githubPAT, githubPAT},
		{"slack_token", `slack.WebClient(token="` + slack + `")`, slack},
		{"google_api_key", "https://maps.googleapis.com/maps/api/js?key=" + google + "&v=3", google},
		{"stripe_key", "stripe.api_key = '" + stripe + "'", stripe},
		{"jwt", "Authorization: Bearer " + jwt, jwt},
		{"private_key", "here is the key:\n" + pem + "\nkeep it safe", pem},
		{"url_password", "DATABASE_URL=postgres://app:" + dbPass + "@db.internal:5432/orders", dbPass},
		{"url_password", `redis.from_url("redis://default:` + dbPass + `@cache:6379/0")`, dbPass},
		{"assigned_secret", `password = "` + generic + `"`, generic},
		{"assigned_secret", `"db_password": "` + generic + `",`, generic},
		{"assigned_secret", "API_KEY=" + generic, generic},
		{"assigned_secret", "client_secret: " + generic, generic},
		{"assigned_secret", "  auth_token: '" + generic + "'", generic},
	}
}

// negatives are things that look a little like secrets and are not. A
// detector that flags these gets switched off.
func negatives() []string {
	return []string{
		// Hashes and ids.
		"commit 9fceb02d0ae598e95dc970b74767f19372d61af8",
		"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"request id 7f3a91c2-4b1e-4d8a-9c3f-2e5a6b7c8d9e",
		`"X-Claude-Code-Session-Id": "cf22532c-18b1-4a5b-b759-c69139caa14d"`,
		`"id": "toolu_01KaYPegFfLqnUt97Sd6MWGp"`,
		`"id": "msg_01XFDUDYJgAACzvnptvVoYEL"`,
		// Placeholders and references to secrets held elsewhere.
		`password = "changeme"`,
		`api_key = "your-api-key-here"`,
		"API_KEY=${API_KEY}",
		"token: <your-token>",
		`password = os.environ["DB_PASSWORD"]`,
		`secret: "{{ .Values.secret }}"`,
		"password: ********",
		`api_key="xxxxxxxxxxxxxxxx"`,
		"export TOKEN=$(vault read -field=token secret/app)",
		`"password": null`,
		// Code that merely mentions secret-sounding names.
		"token = parseToken(input)",
		"password := getPasswordFromKeychain()",
		"func validateApiKey(apiKey string) error {",
		"if token == expectedToken {",
		"secret_key_base: config.secretKeyBase",
		`access_key = settings.ACCESS_KEY`,
		"the api_key: field is required",
		"max_tokens: 128000",
		"password_min_length = 12",
		`token_type: "bearer"`,
		// URLs without credentials, and with a user but no password.
		"https://api.anthropic.com/v1/messages?beta=true",
		"postgres://db.internal:5432/orders",
		"ssh://git@github.com/acme/app.git",
		"https://user@example.com/path",
		// Prefixes that are not keys.
		"sk-learn is the package name",
		"the task-ant-colony optimiser",
		"AKIA is how AWS key ids begin",
		"gh_pages branch",
		"eyJ is how a JWT begins",
		// Long ordinary strings.
		"internal/proxy/proxy_test.go:TestReportsMessagesUsageWithoutChangingTheStream",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"The quick brown fox jumps over the lazy dog and keeps on running",
		"-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE\n-----END PUBLIC KEY-----",
	}
}

// entropyOnly are secrets with no recognisable format and no telling name
// beside them. Only the entropy search can find these.
func entropyOnly() []string {
	return []string{
		"the value is " + rnd[:40] + " as before",
		"curl -H 'X-Auth: " + rnd[20:68] + "' https://internal/api",
	}
}

// entropyTraps are random-looking strings that are not secrets. The
// entropy search flags them; this is the price of switching it on.
func entropyTraps() []string {
	return []string{
		`"integrity": "sha512-` + rnd[:86] + `=="`,                                                         // a lockfile checksum
		"data:image/png;base64,iVBORw0KGgo" + rnd[:60],                                                     // an embedded image
		`"signature": "` + rnd[10:74] + `"`,                                                                // a signed thinking block
		"webpack://app/./node_modules/" + rnd[:40] + "/index.js",                                           // a generated path
		"-----BEGIN CERTIFICATE-----\nMIIBkTCB+wIJAKHBfpegPjMCMA0GCSqGSIb3DQEB\n-----END CERTIFICATE-----", // public, by definition
	}
}

func TestFindsEveryKnownFormat(t *testing.T) {
	d := &Detector{}
	for _, p := range positives() {
		found := d.Scan(p.text)
		if len(found) != 1 {
			t.Errorf("%s in %.50q: %d findings, want 1: %+v", p.kind, p.text, len(found), found)
			continue
		}
		if got := found[0].Value(p.text); got != p.want || found[0].Kind != p.kind {
			t.Errorf("%s in %.50q:\n found %s %.40q\n  want %s %.40q", p.kind, p.text, found[0].Kind, got, p.kind, p.want)
		}
	}
}

func TestStaysQuietOnLookalikes(t *testing.T) {
	// Without the entropy search the detector is quiet on all of these,
	// the random-looking ones included.
	d := &Detector{}
	for _, text := range append(negatives(), entropyTraps()...) {
		if found := d.Scan(text); len(found) != 0 {
			t.Errorf("false positive in %q: %s %q", text, found[0].Kind, found[0].Value(text))
		}
	}
}

// The numbers docs/GUARD.md quotes come from here. Run with -v to see them.
func TestMeasureOnCorpus(t *testing.T) {
	measure := func(d *Detector) (recall, falsePositives int) {
		for _, p := range positives() {
			for _, f := range d.Scan(p.text) {
				if f.Value(p.text) == p.want {
					recall++
					break
				}
			}
		}
		for _, text := range append(negatives(), entropyTraps()...) {
			falsePositives += len(d.Scan(text))
		}
		return recall, falsePositives
	}
	entropyFound := func(d *Detector) (n int) {
		for _, text := range entropyOnly() {
			if len(d.Scan(text)) > 0 {
				n++
			}
		}
		return n
	}
	total, lookalikes := len(positives()), len(negatives())+len(entropyTraps())

	plain, withEntropy := &Detector{}, &Detector{Entropy: true}
	recall, fp := measure(plain)
	t.Logf("formats only:   %d/%d secrets found, %d false positives on %d lookalikes, %d/%d unformatted secrets found",
		recall, total, fp, lookalikes, entropyFound(plain), len(entropyOnly()))
	if recall != total || fp != 0 {
		t.Errorf("without the entropy search: recall %d/%d, %d false positives; want all found and none false", recall, total, fp)
	}

	recallE, fpE := measure(withEntropy)
	t.Logf("with entropy:   %d/%d secrets found, %d false positives on %d lookalikes, %d/%d unformatted secrets found",
		recallE, total, fpE, lookalikes, entropyFound(withEntropy), len(entropyOnly()))
	if recallE != total || entropyFound(withEntropy) != len(entropyOnly()) {
		t.Errorf("with the entropy search: recall %d/%d, unformatted %d/%d", recallE, total, entropyFound(withEntropy), len(entropyOnly()))
	}
	if fpE == 0 {
		t.Error("the entropy search flagged none of the traps; the documentation says it does, so one of them is wrong")
	}
	if fpE > len(entropyTraps()) {
		t.Errorf("the entropy search has %d false positives, more than the %d traps it is known to fall for", fpE, len(entropyTraps()))
	}
}

func TestSeveralSecretsInOneText(t *testing.T) {
	ps := positives()
	text := ps[0].text + "\n" + ps[3].text + "\n# nothing here\n" + ps[14].text
	found := (&Detector{}).Scan(text)
	if len(found) != 3 {
		t.Fatalf("%d findings, want 3: %+v", len(found), found)
	}
	for i, want := range []positive{ps[0], ps[3], ps[14]} {
		if found[i].Kind != want.kind || found[i].Value(text) != want.want {
			t.Errorf("finding %d = %s %.30q, want %s", i, found[i].Kind, found[i].Value(text), want.kind)
		}
		if i > 0 && found[i].Start < found[i-1].End {
			t.Errorf("findings %d and %d overlap", i-1, i)
		}
	}
}

// A key that also sits behind a secret-sounding name is one finding, not
// two, and is called by its specific name.
func TestOverlappingMatchesAreOneFinding(t *testing.T) {
	key := positives()[0].want
	text := `api_key = "` + key + `"`
	found := (&Detector{}).Scan(text)
	if len(found) != 1 || found[0].Value(text) != key || found[0].Kind != "anthropic_api_key" {
		t.Errorf("findings = %+v", found)
	}
}

func TestCustomPatternsAndAllowList(t *testing.T) {
	d := &Detector{Patterns: []*regexp.Regexp{
		regexp.MustCompile(`ACME-[0-9]{6}-[A-Z]{4}`),
		regexp.MustCompile(`licence:\s*(\S+)`), // a group marks the secret
	}}
	text := "order ACME-204857-QRZX shipped; licence: L1c3nc3K3y9 expires soon"
	found := d.Scan(text)
	if len(found) != 2 || found[0].Kind != "custom" || found[0].Value(text) != "ACME-204857-QRZX" || found[1].Value(text) != "L1c3nc3K3y9" {
		t.Errorf("findings = %+v", found)
	}

	// An allowed value is never a finding, whichever detector matched it.
	awsID := positives()[3].want
	text = "aws_access_key_id = " + awsID
	if len((&Detector{}).Scan(text)) != 1 {
		t.Fatal("test is wrong: the key should be found without the allow list")
	}
	if found := (&Detector{Allow: []string{awsID, ""}}).Scan(text); len(found) != 0 {
		t.Errorf("an allowed value was reported: %+v", found)
	}
	// Allowing one value does not allow others.
	other := j("AKIA", "ZZZZ2LP9XVB4NKD8")
	if found := (&Detector{Allow: []string{awsID}}).Scan("key " + other); len(found) != 1 {
		t.Errorf("a different key was allowed too: %+v", found)
	}
}

func TestEntropy(t *testing.T) {
	for s, atLeast := range map[string]float64{rnd[:40]: 4.5, "aaaaaaaa": -1, "abababab": 0.9} {
		got := entropy(s)
		if atLeast < 0 && got != 0 {
			t.Errorf("entropy(%q) = %.2f, want 0", s, got)
		}
		if atLeast > 0 && got < atLeast {
			t.Errorf("entropy(%q) = %.2f, want at least %.1f", s, got, atLeast)
		}
	}
	if entropy("") != 0 {
		t.Error("entropy of nothing should be 0")
	}
}

func TestScanHandlesLargeInput(t *testing.T) {
	key := positives()[0].want
	text := strings.Repeat("ordinary log line with nothing in it\n", 20000) + "KEY=" + key + "\n" + strings.Repeat(fmt.Sprintf("more %d\n", 7), 20000)
	found := (&Detector{Entropy: true}).Scan(text)
	if len(found) != 1 || found[0].Value(text) != key {
		t.Errorf("%d findings in a large text, want the one key", len(found))
	}
}
