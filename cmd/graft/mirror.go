package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/feder-cr/git-ai/sdk"
)

// mirrorSynopsis is the command line of graft mirror.
const mirrorSynopsis = "usage: graft mirror push --repo REPO --url URL --branch PATTERN [--branch PATTERN]... [--json]"

// mirrorUsage lists the subcommands of graft mirror.
const mirrorUsage = "mirror requires a subcommand: push (" + mirrorSynopsis + ")"

// mirrorHelp explains graft mirror below its synopsis.
const mirrorHelp = `Publishes selected branches of a Graft repository to another Git host as a
mirror. One command serves every host: there is no per-provider subcommand,
and the host is only the URL, so anything that speaks Git over HTTPS, SSH or
the scp-like form works the same way. The mirror is the compatibility path for
the workflows Graft does not replace yet, such as a pipeline that already runs
there.

The server makes the Git connection; the shared (operator) token authorizes
the RPC, so the remote credentials stay in the server process and never appear
in the command line or its output.

Credentials come from the environment, one variable per run:

  gitee.com   GITEE_TOKEN                    user "oauth2"
  github.com  GITHUB_TOKEN                   user "x-access-token"
  gitlab.com  GITLAB_TOKEN                   user "oauth2"

Those rows are a convenience, not a list of supported products: the host of
--url only picks a default variable and user, so the usual case needs no
credential flags at all. --token-env and --username name the credential for
any other host; the value of the variable is sent as HTTP Basic
authentication. --ssh-key (with --known-hosts) is the SSH alternative, and
a public mirror needs neither. An http:// URL with a token needs
--allow-insecure-http.

--branch is an allowlist and may be repeated; its patterns contain no
refs/heads/ prefix and may use *. Tags are always included, because they carry
the commits the branches name. Nothing under refs/graft/ is published: the
mirror is history, not Graft's review or verification state.

The push is a reconciliation, not an event: it compares the references again
on every run, so a scheduler can retry it after a network failure. By default
a remote branch whose history is not an ancestor of the Graft branch is kept
and reported, and the command exits 1. Inspect it first; --force says that the
Graft branch is the history to keep. Exit status: 0 when the selected
references are on the mirror, 1 when a divergent ref was kept, 2 when the
command line is wrong.`

// mirrorFlags holds the flags of graft mirror push.
type mirrorFlags struct {
	repo              string
	url               string
	branches          stringList
	tokenEnv          string
	username          string
	sshKey            string
	knownHosts        string
	force             bool
	allowInsecureHTTP bool
}

// mirrorResult is the JSON output of graft mirror push.
type mirrorResult struct {
	// Repository is the Graft repository that was mirrored.
	Repository string `json:"repository"`
	// URL is the Git URL of the mirror.
	URL string `json:"url"`
	// Branches are the branch patterns the caller selected.
	Branches []string `json:"branches"`
	// Force tells whether divergent remote branches were overwritten.
	Force bool `json:"force,omitempty"`
	// DivergentRefs are the branches the server kept on the mirror.
	DivergentRefs []string `json:"divergent_refs,omitempty"`
}

// mirrorCommand returns the mirror command, the operator's push of a Graft
// repository to another Git server for the workflows that still run there.
func mirrorCommand() command {
	f := &mirrorFlags{}
	return command{
		bind: func(fs *flag.FlagSet, o *options) {
			fs.Usage = func() {
				fmt.Fprintf(fs.Output(), "%s\n\n%s\n\nflags:\n", mirrorSynopsis, mirrorHelp)
				fs.PrintDefaults()
			}
			fs.StringVar(&f.repo, "repo", "", "repository path, used with -address and -storage")
			fs.StringVar(&f.url, "url", "", "Git URL of the mirror: an HTTPS or SSH URL of any host, or a local path")
			fs.Var(&f.branches, "branch", "branch pattern to push (repeatable, * is a wildcard)")
			fs.StringVar(&f.tokenEnv, "token-env", "", "environment variable that holds the HTTPS token (default: the host's usual variable, such as GITHUB_TOKEN)")
			fs.StringVar(&f.username, "username", "", `HTTP Basic user for the token (default: by host, such as "oauth2")`)
			fs.StringVar(&f.sshKey, "ssh-key", "", "private key for an SSH remote")
			fs.StringVar(&f.knownHosts, "known-hosts", "", "host identities for strict SSH host-key checking")
			fs.BoolVar(&f.force, "force", false, "overwrite a divergent remote branch with the Graft branch")
			fs.BoolVar(&f.allowInsecureHTTP, "allow-insecure-http", false, "allow an http:// URL with a token")
			fs.BoolVar(&o.jsonOut, "json", false, "print the result as one JSON object")
		},
		run: func(ctx context.Context, out io.Writer, o *options, args []string) error {
			return cmdMirror(ctx, out, o, f, args)
		},
	}
}

// mirrorCredential is what a known Git host calls its token and the HTTP Basic
// user that goes with it. It is a default, never a restriction: any URL works,
// and --token-env and --username override the entry.
type mirrorCredential struct {
	tokenEnv string
	username string
}

// mirrorCredentials are the hosts whose token convention is well known. A host
// that is not listed takes --token-env and --username as given, with "oauth2"
// as the user, which is what most installations accept.
var mirrorCredentials = map[string]mirrorCredential{
	"gitee.com":  {tokenEnv: "GITEE_TOKEN", username: "oauth2"},
	"github.com": {tokenEnv: "GITHUB_TOKEN", username: "x-access-token"},
	"gitlab.com": {tokenEnv: "GITLAB_TOKEN", username: "oauth2"},
}

// mirrorHost returns the host of a Git URL, empty when the URL has none. It
// understands https://host/path, ssh://git@host/path and the scp-like
// git@host:path form.
func mirrorHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return strings.ToLower(u.Hostname())
	}
	// scp-like: [user@]host:path
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		rest := raw[at+1:]
		if colon := strings.Index(rest, ":"); colon > 0 {
			return strings.ToLower(rest[:colon])
		}
	}
	return ""
}

// mirrorDefaults returns the token variable and the Basic user for a URL: the
// host convention, or the generic one when the host is not known.
func mirrorDefaults(raw string) mirrorCredential {
	if c, ok := mirrorCredentials[mirrorHost(raw)]; ok {
		return c
	}
	return mirrorCredential{username: "oauth2"}
}

// cmdMirror dispatches a mirror subcommand. The command line is checked before
// the server is contacted.
func cmdMirror(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, args []string) error {
	if len(args) == 0 {
		return usagef("%s", mirrorUsage)
	}
	if len(args) != 1 || args[0] != "push" {
		return usagef("%s", mirrorSynopsis)
	}
	if f.repo == "" {
		return usagef("mirror push requires --repo REPO")
	}
	if f.url == "" {
		return usagef("mirror push requires --url URL")
	}
	if len(f.branches) == 0 {
		return usagef("mirror push requires --branch PATTERN: name the branches the mirror may publish")
	}
	if f.sshKey != "" && f.tokenEnv != "" {
		return usagef("mirror push takes either --token-env or --ssh-key, not both")
	}
	// A token over plain HTTP is sent in cleartext to every hop. The check is
	// here, before the server is contacted, so that a misconfigured schedule
	// fails with the usage status and sends nothing.
	if name, token := mirrorToken(f); token != "" && strings.HasPrefix(f.url, "http://") && !f.allowInsecureHTTP {
		return usagef("mirror push refuses to send %s over http://%s: use HTTPS, or pass --allow-insecure-http", name, strings.TrimPrefix(f.url, "http://"))
	}
	if _, err := mirrorPush(ctx, out, o, f); err != nil {
		return err
	}
	return nil
}

// mirrorPush asks the server to reconcile the selected references with the
// mirror.
func mirrorPush(ctx context.Context, out io.Writer, o *options, f *mirrorFlags) (*mirrorResult, error) {
	repo, _, _, err := gateRepo(ctx, o, &gateFlags{repo: f.repo})
	if err != nil {
		return nil, err
	}

	authorization, err := mirrorAuthorization(f)
	if err != nil {
		return nil, err
	}

	res := &mirrorResult{
		Repository: f.repo,
		URL:        f.url,
		Branches:   append([]string(nil), f.branches...),
		Force:      f.force,
	}
	answer, err := repo.Mirror(ctx, sdk.MirrorOptions{
		URL:                     f.url,
		Branches:                f.branches,
		HTTPAuthorizationHeader: authorization,
		SSHKey:                  f.sshKey,
		KnownHosts:              f.knownHosts,
		// The default is the safety net: a mirror that has unrelated history
		// is a person's decision, not a retry's.
		KeepDivergentRefs: !f.force,
	})
	if err != nil {
		return nil, err
	}
	res.DivergentRefs = append(res.DivergentRefs, answer.DivergentRefs...)

	if o.jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return nil, err
		}
	} else {
		fmt.Fprintf(out, "mirrored %s to %s (branches: %s)\n", f.repo, mirrorDisplayURL(f.url), strings.Join(f.branches, ", "))
		for _, ref := range res.DivergentRefs {
			fmt.Fprintf(out, "kept the divergent ref %s\n", ref)
		}
		if len(res.DivergentRefs) > 0 {
			fmt.Fprintln(out, "the mirror is incomplete: inspect it, then repair it or run again with --force")
		}
	}

	if len(res.DivergentRefs) > 0 {
		return res, errors.New(strings.Join(res.DivergentRefs, ", "))
	}
	return res, nil
}

// mirrorAuthorization builds the Authorization header from the environment.
// The token itself is never printed, and never reaches argv.
func mirrorAuthorization(f *mirrorFlags) (string, error) {
	if f.sshKey != "" {
		// The caller chose the SSH identity: no HTTP header is built.
		return "", nil
	}
	tokenEnv, token := mirrorToken(f)
	if token == "" {
		return "", nil
	}
	if !strings.HasPrefix(f.url, "https://") && !strings.HasPrefix(f.url, "http://") {
		return "", usagef("mirror push: %s holds an HTTP token, but %q is not an HTTP URL (use --ssh-key for SSH)", tokenEnv, f.url)
	}
	defaults := mirrorDefaults(f.url)
	user := f.username
	if user == "" {
		user = defaults.username
	}
	if user == "" {
		user = "oauth2"
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token)), nil
}

// mirrorToken returns the token variable of the run and its value: the one the
// caller named, else the one the host of the URL implies, and empty when the
// variable is unset.
func mirrorToken(f *mirrorFlags) (string, string) {
	name := f.tokenEnv
	if name == "" {
		name = mirrorDefaults(f.url).tokenEnv
	}
	if name == "" {
		return "", ""
	}
	return name, os.Getenv(name)
}

// mirrorDisplayURL hides the user information of a URL that a person may have
// put a credential in: the mirror is printed in logs and chat.
func mirrorDisplayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}
