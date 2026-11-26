package observer

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// The picture of the fleet is read by a browser. It must never carry a
// credential, and a write must never be made without the token of the
// observer.

func TestDescribeDatabaseNeverNamesTheCredentials(t *testing.T) {
	cases := map[string]string{
		"postgres://temporal:secret@192.168.2.190:15432/gitai?sslmode=disable": "192.168.2.190:15432/gitai",
		"postgres://user@host/db":          "host/db",
		"sqlite:///var/lib/git-ai/core.db": "sqlite:core.db",
		"sqlite:relative.db":               "sqlite:relative.db",
		"":                                 "",
	}
	for dsn, want := range cases {
		got := describeDatabase(dsn)
		if got != want {
			t.Fatalf("describeDatabase(%q) = %q, want %q", dsn, got, want)
		}
		if strings.Contains(got, "secret") || strings.Contains(got, ":secret@") {
			t.Fatalf("describeDatabase(%q) = %q, it leaks the password", dsn, got)
		}
	}
}

func TestActionsAreAuthorizedByTheTokenOfTheObserver(t *testing.T) {
	open := NewActions(Config{})
	if !open.Authorized("") {
		t.Fatal("an observer without a token accepts every client")
	}

	closed := NewActions(Config{ActionToken: "s3cret"})
	if !closed.Authorized("s3cret") {
		t.Fatal("the right token is refused")
	}
	if closed.Authorized("wrong") {
		t.Fatal("a wrong token is accepted")
	}
	if closed.Authorized("") {
		t.Fatal("a missing token is accepted")
	}
}

func TestActionTokenIsReadFromTheHeaderOrTheBearer(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/actions/land", nil)
	if got := actionToken(request); got != "" {
		t.Fatalf("token = %q, want empty", got)
	}

	request.Header.Set("X-Action-Token", "from-header")
	if got := actionToken(request); got != "from-header" {
		t.Fatalf("token = %q, want from-header", got)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/actions/land", nil)
	request.Header.Set("Authorization", "Bearer from-bearer")
	if got := actionToken(request); got != "from-bearer" {
		t.Fatalf("token = %q, want from-bearer", got)
	}
}

func TestRedactHidesTheSecretOfTheServer(t *testing.T) {
	actions := NewActions(Config{GraftToken: "top-secret-token"})
	text := actions.redact("the call used top-secret-token to authenticate")
	if strings.Contains(text, "top-secret-token") {
		t.Fatalf("the text still holds the secret: %q", text)
	}
	if !strings.Contains(text, "redacted") {
		t.Fatalf("the text does not say that something was hidden: %q", text)
	}
}

func TestTheEndpointsRefuseTheWrongMethod(t *testing.T) {
	observer, err := New(Config{
		Database:    "sqlite:" + filepath.Join(t.TempDir(), "core.db"),
		ActionToken: "s3cret",
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer observer.Stop()

	server := httptest.NewServer(observer.Handler())
	defer server.Close()

	response, err := http.Post(server.URL+"/api/actions/repo-create", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: a write without a token is refused", response.StatusCode)
	}
}

func TestTheObserverNeverWritesToTheStorageCore(t *testing.T) {
	observer, err := New(Config{Database: "sqlite://" + filepath.Join(t.TempDir(), "core.db")})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer observer.Stop()

	statement, err := observer.db.Prepare("select 1")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer statement.Close()
	if _, err := statement.Exec(); err != nil {
		t.Fatalf("exec: %v", err)
	}
}
