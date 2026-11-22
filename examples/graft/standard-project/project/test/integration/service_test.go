//go:build integration

// Package integration starts the built service as a real process and talks to
// it over a real socket: it answers "does the service still behave the way a
// client sees it", which a unit test of the handler cannot.
package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// startShopd builds cmd/shopd, runs it on a port the kernel picks and returns
// its base URL. The cleanup stops the process.
func startShopd(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "shopd")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "./cmd/shopd")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the service: %v\n%s", err, out)
	}

	cmd := exec.Command(bin, "-listen", "127.0.0.1:0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe the stdout of the service: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the service: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// The service prints the address it bound before it serves, and the socket
	// accepts connections from the moment it is bound, so the first request
	// queues instead of failing.
	line := bufio.NewScanner(stdout)
	if !line.Scan() {
		t.Fatalf("the service printed no address: %s", stderr.String())
	}
	return "http://" + line.Text()
}

func TestTheServiceAddsUpALine(t *testing.T) {
	base := startShopd(t)
	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Get(base + "/total?sku=tea&qty=3&cents=250")
	if err != nil {
		t.Fatalf("GET /total: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /total answered %s", resp.Status)
	}
	var body struct {
		Cents int `json:"cents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode the answer: %v", err)
	}
	if body.Cents != 750 {
		t.Fatalf("the service answered %d cents, want 750", body.Cents)
	}
}

func TestTheServiceReportsItsHealthAndRefusesGarbage(t *testing.T) {
	base := startShopd(t)
	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz answered %s", resp.Status)
	}

	resp, err = client.Get(base + "/total?sku=tea&qty=nine&cents=250")
	if err != nil {
		t.Fatalf("GET /total with a bad quantity: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET /total with a bad quantity answered %s, want 400", resp.Status)
	}
}

func TestMain(m *testing.M) {
	// Keep `go test` from inheriting a GIT_DIR that belongs to another
	// repository: the build of the service shells out to git.
	os.Unsetenv("GIT_DIR")
	fmt.Fprintln(os.Stderr, "integration: the service is started as a real process")
	os.Exit(m.Run())
}
