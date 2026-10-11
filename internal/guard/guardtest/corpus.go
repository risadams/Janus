// Package guardtest holds the corpus of invented secrets that the guard's
// tests, and the leak tests, are run against.
//
// Every secret here is made up, and assembled from pieces at run time so
// that no credential-shaped string appears in the repository for a
// scanner to trip over.
package guardtest

import "strings"

func j(parts ...string) string { return strings.Join(parts, "") }

// Rnd is a fixed string of mixed characters to cut secrets from.
const Rnd = "q7Rz2Lp9XvB4nKd8Yt3WmHs6JcF5gPa1ZeUo0QiNxTbVrMlCkEyDwGhSuAfIjO7q2Rz9Lp4XvB8nKd3Yt6WmHs5JcF1gPa0ZeUoQiNxTb"

// A Secret is one invented secret in the kind of text it turns up in.
type Secret struct {
	Kind string
	Text string // the text to scan
	Want string // the secret that should be found in it
}

// Secrets returns the corpus: at least one of every kind the guard knows.
func Secrets() []Secret {
	const rnd = Rnd
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

	return []Secret{
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
		{"assigned_secret", j(`pass`, `word = "`) + generic + `"`, generic},
		{"assigned_secret", j(`"db_pass`, `word": "`) + generic + `",`, generic},
		{"assigned_secret", j("API_", "KEY=") + generic, generic},
		{"assigned_secret", j("client_", "secret: ") + generic, generic},
		{"assigned_secret", j("  auth_", "token: '") + generic + "'", generic},
	}
}
