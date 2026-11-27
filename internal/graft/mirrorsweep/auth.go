package mirrorsweep

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Credential is what a known Git host calls its token and the HTTP Basic user
// that goes with it. It is a default, never a restriction: any URL works, and
// the caller may override the entry with its own token variable and user.
type Credential struct {
	// TokenEnv is the name of the environment variable that holds the token.
	TokenEnv string
	// Username is the HTTP Basic user of the token.
	Username string
}

// credentials are the hosts whose token convention is well known. A host that
// is not listed takes the token variable and the user as given, with "oauth2"
// as the user, which is what most installations accept.
var credentials = map[string]Credential{
	"gitee.com":  {TokenEnv: "GITEE_TOKEN", Username: "oauth2"},
	"github.com": {TokenEnv: "GITHUB_TOKEN", Username: "x-access-token"},
	"gitlab.com": {TokenEnv: "GITLAB_TOKEN", Username: "oauth2"},
}

// Host returns the host of a Git URL, empty when the URL has none. It
// understands https://host/path, ssh://git@host/path and the scp-like
// git@host:path form.
func Host(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return strings.ToLower(u.Hostname())
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	}
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.Index(raw, "@"); i >= 0 {
		raw = raw[i+1:]
	}
	if i := strings.Index(raw, ":"); i >= 0 {
		raw = raw[:i]
	}
	return strings.ToLower(raw)
}

// Defaults returns the credential convention of the host of a URL.
func Defaults(raw string) Credential {
	if c, ok := credentials[Host(raw)]; ok {
		return c
	}
	return Credential{Username: "oauth2"}
}

// Auth is the credential of one mirror: the URL it publishes to, the name of
// the environment variable that holds the token, the HTTP Basic user of the
// token, and the private key of an SSH remote. The push and the queue build
// one, so that both reach a host the same way.
type Auth struct {
	// Usage names the command in a refusal, so that the reason says which
	// command refused.
	Usage string
	// URL is the Git URL of the mirror.
	URL string
	// TokenEnv is the name of the environment variable that holds the token.
	TokenEnv string
	// Username is the HTTP Basic user of the token.
	Username string
	// SSHKey is the path of the private key of an SSH remote.
	SSHKey string
}

// Authorization builds the Authorization header from the environment. The
// token itself is never printed, and never reaches argv.
func Authorization(a Auth) (string, error) {
	if a.SSHKey != "" {
		// The caller chose the SSH identity: no HTTP header is built.
		return "", nil
	}
	tokenEnv, token := Token(a)
	if tokenEnv != "" && token == "" {
		// The host of the URL has a convention for its token, or the caller
		// named one. A mirror that needs a credential and does not get one
		// would fail later, with the error of the host in the way. The name
		// of the variable is what the operator needs.
		return "", fmt.Errorf("%s: %s is not set in the environment", a.Usage, tokenEnv)
	}
	if token == "" {
		return "", nil
	}
	if !strings.HasPrefix(a.URL, "https://") && !strings.HasPrefix(a.URL, "http://") {
		return "", fmt.Errorf("%s: %s holds an HTTP token, but %q is not an HTTP URL (use --ssh-key for SSH)", a.Usage, tokenEnv, a.URL)
	}
	defaults := Defaults(a.URL)
	user := a.Username
	if user == "" {
		user = defaults.Username
	}
	if user == "" {
		user = "oauth2"
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token)), nil
}

// Token returns the token variable of the run and its value: the one the
// caller named, else the one the host of the URL implies, and empty when the
// variable is unset.
func Token(a Auth) (string, string) {
	name := a.TokenEnv
	if name == "" {
		name = Defaults(a.URL).TokenEnv
	}
	if name == "" {
		return "", ""
	}
	return name, os.Getenv(name)
}

// CredentialFile reads a credential file that the caller named by path, such
// as --ssh-key or --known-hosts. The request of the server carries the
// contents, because the server writes them into a private file of its own and
// lets the Git connection use it for the length of the call. The read happens
// in the client so that the path is resolved where the person wrote it, and a
// path that is wrong fails before any Git connection is made.
func CredentialFile(usage, flag, path string) (string, error) {
	if path == "" {
		return "", nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %s %s: %w", usage, flag, path, err)
	}
	if strings.TrimSpace(string(content)) == "" {
		return "", fmt.Errorf("%s: %s %s is empty", usage, flag, path)
	}
	return string(content), nil
}

// DisplayURL hides the user information of a URL that a person may have put a
// credential in: the mirror is printed in logs and chat.
func DisplayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}
