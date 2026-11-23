package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// brokenURL is an address no HTTP request can be built from, so the request
// itself fails before it leaves the process.
const brokenURL = "http://exa mple.com"

// failingReader fails on the first read. It stands for a connection that
// breaks while the body is read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("the connection broke")
}

func (failingReader) Close() error { return nil }

// TestReleaseVersionReportsTheFailuresOfTheAPI covers the failures of the
// release API: a request that cannot be built, a transport that fails, a
// server that refuses, an answer that is not JSON and an answer that names no
// release.
func TestReleaseVersionReportsTheFailuresOfTheAPI(t *testing.T) {
	t.Parallel()

	// A request that cannot be built.
	source := &upgradeSource{api: brokenURL}
	_, err := source.releaseVersion(t.Context(), "oliveagle/git-ai", "releases/latest")
	require.Error(t, err)
	require.Contains(t, err.Error(), "release request")

	// A transport that fails.
	source = &upgradeSource{api: "http://example.invalid", do: func(*http.Request) (*http.Response, error) {
		return nil, errors.New("the network is down")
	}}
	_, err = source.releaseVersion(t.Context(), "oliveagle/git-ai", "releases/latest")
	require.Error(t, err)
	require.Contains(t, err.Error(), "release request")
	require.Contains(t, err.Error(), "the network is down")

	// A server that refuses the request.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such release", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	source = &upgradeSource{api: server.URL, do: server.Client().Do}
	_, err = source.releaseVersion(t.Context(), "oliveagle/git-ai", "releases/latest")
	require.Error(t, err)
	require.Contains(t, err.Error(), "answered")

	// An answer that is not JSON.
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("this is not JSON"))
	}))
	t.Cleanup(server.Close)
	source = &upgradeSource{api: server.URL, do: server.Client().Do}
	_, err = source.releaseVersion(t.Context(), "oliveagle/git-ai", "releases/latest")
	require.Error(t, err)
	require.Contains(t, err.Error(), "release of oliveagle/git-ai")

	// An answer that names no release.
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name": "no tag here"}`))
	}))
	t.Cleanup(server.Close)
	source = &upgradeSource{api: server.URL, do: server.Client().Do}
	_, err = source.releaseVersion(t.Context(), "oliveagle/git-ai", "releases/latest")
	require.Error(t, err)
	require.Contains(t, err.Error(), "answers no tag")
}

// TestReleaseVersionSendsTheTokenOfGitHub covers the token the release API
// accepts to raise the rate limit of an anonymous client.
func TestReleaseVersionSendsTheTokenOfGitHub(t *testing.T) {
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"tag_name": "v0.9.0"}`))
	}))
	t.Cleanup(server.Close)

	t.Setenv("GITHUB_TOKEN", "a-token-of-the-test")
	source := &upgradeSource{api: server.URL, do: server.Client().Do}
	version, err := source.releaseVersion(t.Context(), "oliveagle/git-ai", "releases/latest")
	require.NoError(t, err)
	require.Equal(t, "0.9.0", version)
	require.Equal(t, "Bearer a-token-of-the-test", seen)
}

// TestFetchReportsTheFailuresOfTheDownload covers the failures of the
// download: a request that cannot be built, a transport that fails, a server
// that refuses and a body that breaks while it is read.
func TestFetchReportsTheFailuresOfTheDownload(t *testing.T) {
	t.Parallel()

	source := &upgradeSource{files: brokenURL}
	_, err := source.fetch(t.Context(), brokenURL+"/pkg")
	require.Error(t, err)
	require.Contains(t, err.Error(), "download")

	source = &upgradeSource{do: func(*http.Request) (*http.Response, error) {
		return nil, errors.New("the network is down")
	}}
	_, err = source.fetch(t.Context(), "http://example.invalid/pkg")
	require.Error(t, err)
	require.Contains(t, err.Error(), "the network is down")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	source = &upgradeSource{do: server.Client().Do}
	_, err = source.fetch(t.Context(), server.URL+"/pkg")
	require.Error(t, err)
	require.Contains(t, err.Error(), "answered")

	source = &upgradeSource{do: func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(failingReader{}),
		}, nil
	}}
	_, err = source.fetch(t.Context(), "http://example.invalid/pkg")
	require.Error(t, err)
	require.Contains(t, err.Error(), "the connection broke")
}

// tarPackageWithModes builds a tarball whose entries carry the given modes.
func tarPackageWithModes(t *testing.T, modes map[string]int64) []byte {
	t.Helper()

	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	archive := tar.NewWriter(gz)
	for name, mode := range modes {
		require.NoError(t, archive.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: 1, Typeflag: tar.TypeReg}))
		_, err := archive.Write([]byte("x"))
		require.NoError(t, err)
	}
	require.NoError(t, archive.Close())
	require.NoError(t, gz.Close())
	return raw.Bytes()
}

// TestUnpackToolsKeepsTheToolsExecutable covers the modes of the unpacked
// tools: a tool of a package without an executable bit keeps one, and a tool
// without any mode is written as an executable one.
func TestUnpackToolsKeepsTheToolsExecutable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	packageData := tarPackageWithModes(t, map[string]int64{"./graft": 0o644})
	written, err := unpackTools(packageData, false, dir, []string{"graft"})
	require.NoError(t, err)
	require.Equal(t, []string{"graft"}, written)

	info, err := os.Stat(filepath.Join(dir, "graft"))
	require.NoError(t, err)
	require.NotZero(t, info.Mode().Perm()&0o100, "the tool keeps an executable bit")

	// A tool without any mode is written as an executable one.
	dir = t.TempDir()
	packageData = tarPackageWithModes(t, map[string]int64{"./graft": 0})
	written, err = unpackTools(packageData, false, dir, []string{"graft"})
	require.NoError(t, err)
	require.Equal(t, []string{"graft"}, written)

	info, err = os.Stat(filepath.Join(dir, "graft"))
	require.NoError(t, err)
	require.NotZero(t, info.Mode().Perm()&0o100, "a tool without a mode is executable")
}

// TestUnpackToolsReportsTheFailuresOfThePackage covers the packages that
// cannot be read: a package that is not an archive, an archive that breaks
// while it is read, an entry that cannot be written and an entry that leaves
// the top level.
func TestUnpackToolsReportsTheFailuresOfThePackage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := unpackTools([]byte("this is not an archive"), false, dir, []string{"graft"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "reading the package")

	_, err = unpackTools([]byte("this is not a zip"), true, dir, []string{"graft"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "reading the package")

	// An archive that breaks while it is read.
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	archive := tar.NewWriter(gz)
	require.NoError(t, archive.WriteHeader(&tar.Header{Name: "./graft", Mode: 0o755, Size: 100, Typeflag: tar.TypeReg}))
	_, _ = archive.Write([]byte("short"))
	require.NoError(t, gz.Close())
	_, err = unpackTools(raw.Bytes(), false, dir, []string{"graft"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "reading")

	// An entry that leaves the top level.
	packageData := tarPackageWithModes(t, map[string]int64{"sub/graft": 0o755})
	_, err = unpackTools(packageData, false, dir, []string{"graft"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a tool of the top level")

	// An entry that cannot be written.
	dir = t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o500))
	packageData = tarPackageWithModes(t, map[string]int64{"./graft": 0o755})
	_, err = unpackTools(packageData, false, dir, []string{"graft"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "writing")
}

// TestUnpackToolsReadsAZipPackage covers the package of Windows, which is a
// zip.
func TestUnpackToolsReadsAZipPackage(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	archive := zip.NewWriter(&buf)
	entry, err := archive.Create("graft")
	require.NoError(t, err)
	_, err = entry.Write([]byte("# the tool\n"))
	require.NoError(t, err)
	require.NoError(t, archive.Close())

	dir := t.TempDir()
	written, err := unpackTools(buf.Bytes(), true, dir, []string{"graft"})
	require.NoError(t, err)
	require.Equal(t, []string{"graft"}, written)

	body, err := os.ReadFile(filepath.Join(dir, "graft"))
	require.NoError(t, err)
	require.Equal(t, "# the tool\n", string(body))
}

// TestUnpackToolsSkipsTheEntriesThatAreNoTools covers the entries of a
// package that carry no tool: they are skipped instead of written.
func TestUnpackToolsSkipsTheEntriesThatAreNoTools(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	packageData := tarPackageWithModes(t, map[string]int64{"./graft": 0o755, "./readme.md": 0o644})
	written, err := unpackTools(packageData, false, dir, []string{"graft"})
	require.NoError(t, err)
	require.Equal(t, []string{"graft"}, written)
}

// TestUpgradeReportsTheFailuresOfTheInstall covers the failures of the
// installation: a directory that cannot be staged in and a package that
// carries fewer tools than the release promises.
func TestUpgradeReportsTheFailuresOfTheInstall(t *testing.T) {
	t.Parallel()

	name, packageData, checksums := releasePackage(t, upgradeTestVersion)
	source := upgradeServer(t, map[string][]byte{name: packageData, "checksums.txt": []byte(checksums)})

	// A directory that does not exist cannot hold the staging directory.
	options := defaultOptions()
	err := runUpgrade(t.Context(), &bytes.Buffer{}, &options, &upgradeFlags{repo: "oliveagle/git-ai"}, source,
		filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "staging the tools")

	// A package that carries fewer tools than the release promises.
	shorter := tarPackageWithModes(t, map[string]int64{
		"./graft":        0o755,
		"./graft-cache":  0o755,
		"./graft-verify": 0o755,
	})
	sum := sha256.Sum256(shorter)
	rows := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name)
	source = upgradeServer(t, map[string][]byte{name: shorter, "checksums.txt": []byte(rows)})

	err = runUpgrade(t.Context(), &bytes.Buffer{}, &options, &upgradeFlags{repo: "oliveagle/git-ai"}, source, t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "expected")
}

// TestUpgradeCheckReportsEveryState covers the states the check reports: a
// build that is on the release, a build that records no version and a build
// that is between two releases.
func TestUpgradeCheckReportsEveryState(t *testing.T) {
	t.Parallel()

	options := defaultOptions()
	var stdout bytes.Buffer
	require.NoError(t, upgradeCheck(&stdout, &options, "0.8.0", "0.8.0"))
	require.Contains(t, stdout.String(), "the tools are on this release")

	stdout.Reset()
	require.NoError(t, upgradeCheck(&stdout, &options, "unknown", "0.8.0"))
	require.Contains(t, stdout.String(), "the build records no version")

	stdout.Reset()
	require.NoError(t, upgradeCheck(&stdout, &options, "0.8.0-2-g1a2b3c4", "0.9.0"))
	require.Contains(t, stdout.String(), "graft upgrade installs 0.9.0")

	options.jsonOut = true
	stdout.Reset()
	require.NoError(t, upgradeCheck(&stdout, &options, "0.8.0", "0.9.0"))
	require.Contains(t, stdout.String(), `"up_to_date":false`)
}

// TestUpgradeUpToDateReportsTheJSON covers the JSON of a build that is
// already on the release.
func TestUpgradeUpToDateReportsTheJSON(t *testing.T) {
	t.Parallel()

	options := defaultOptions()
	options.jsonOut = true
	var stdout bytes.Buffer
	require.NoError(t, upgradeUpToDate(&stdout, &options, "0.8.0", "0.8.0"))
	require.Contains(t, stdout.String(), `"up_to_date":true`)
}

// TestUpgradeReportsTheFailureOfTheReleaseAPI covers the upgrade that cannot
// name the newest release.
func TestUpgradeReportsTheFailureOfTheReleaseAPI(t *testing.T) {
	t.Parallel()

	source := &upgradeSource{api: "http://example.invalid", do: func(*http.Request) (*http.Response, error) {
		return nil, errors.New("the network is down")
	}}
	options := defaultOptions()
	err := runUpgrade(t.Context(), &bytes.Buffer{}, &options, &upgradeFlags{repo: "oliveagle/git-ai"}, source, t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "the network is down")
}

// TestUpgradeFailsOnAPackageThatDoesNotMatchItsChecksum covers the package
// whose SHA-256 does not match the checksums file of the release.
func TestUpgradeFailsOnAPackageThatDoesNotMatchItsChecksum(t *testing.T) {
	t.Parallel()

	name, packageData, _ := releasePackage(t, upgradeTestVersion)
	rows := strings.Repeat("0", 64) + "  " + name + "\n"
	source := upgradeServer(t, map[string][]byte{name: packageData, "checksums.txt": []byte(rows)})

	options := defaultOptions()
	err := runUpgrade(t.Context(), &bytes.Buffer{}, &options, &upgradeFlags{repo: "oliveagle/git-ai"}, source, t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "SHA-256")
}

// TestUpgradeReportsAMalformedChecksumsFile covers the checksums file that
// carries no row at all.
func TestUpgradeReportsAMalformedChecksumsFile(t *testing.T) {
	t.Parallel()

	name, packageData, _ := releasePackage(t, upgradeTestVersion)
	source := upgradeServer(t, map[string][]byte{name: packageData, "checksums.txt": []byte("no rows here\n")})

	options := defaultOptions()
	err := runUpgrade(t.Context(), &bytes.Buffer{}, &options, &upgradeFlags{repo: "oliveagle/git-ai"}, source, t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "checksums.txt")
}

// TestUpgradeTakesTheVersionOfTheCommandLine covers the release the command
// line names instead of the newest one.
func TestUpgradeTakesTheVersionOfTheCommandLine(t *testing.T) {
	t.Parallel()

	name, packageData, checksums := releasePackage(t, upgradeTestVersion)
	source := upgradeServer(t, map[string][]byte{name: packageData, "checksums.txt": []byte(checksums)})

	previous := currentVersion
	currentVersion = func() string { return "0.7.0" }
	t.Cleanup(func() { currentVersion = previous })

	options := defaultOptions()
	var stdout bytes.Buffer
	err := runUpgrade(t.Context(), &stdout, &options, &upgradeFlags{repo: "oliveagle/git-ai", version: "v" + upgradeTestVersion}, source, t.TempDir())
	require.NoError(t, err)
	require.Contains(t, stdout.String(), upgradeTestVersion)
}

// TestUpgradeSourceOfGitHubNamesTheGitHubAPI covers the default source.
func TestUpgradeSourceOfGitHubNamesTheGitHubAPI(t *testing.T) {
	t.Parallel()

	source := defaultUpgradeSource()
	require.Equal(t, "https://api.github.com", source.api)
	require.Equal(t, "https://github.com", source.files)
	require.NotNil(t, source.do)
}

// TestUpgradeContextIsHonoured covers the upgrade that is canceled while it
// waits for the release API.
func TestUpgradeContextIsHonoured(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	source := &upgradeSource{api: "http://example.invalid", do: func(*http.Request) (*http.Response, error) {
		return nil, ctx.Err()
	}}
	options := defaultOptions()
	err := runUpgrade(ctx, &bytes.Buffer{}, &options, &upgradeFlags{repo: "oliveagle/git-ai"}, source, t.TempDir())
	require.Error(t, err)
}
