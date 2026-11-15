package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// upgradeTestVersion is the release the local server of the tests publishes.
const upgradeTestVersion = "0.8.0"

// tarPackage builds the tarball of a release package: one executable entry per
// named body.
func tarPackage(t *testing.T, entries map[string]string) []byte {
	t.Helper()

	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	archive := tar.NewWriter(gz)
	for _, name := range []string{"graft", "graft-cache", "graft-verify", "graft-bench"} {
		body, ok := entries[name]
		require.True(t, ok, "the package misses %s", name)
		require.NoError(t, archive.WriteHeader(&tar.Header{Name: "./" + name, Mode: 0o755, Size: int64(len(body))}))
		_, err := archive.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, archive.Close())
	require.NoError(t, gz.Close())
	return raw.Bytes()
}

// upgradeServer starts a release server that names the newest release and
// serves the files of the map below /OWNER/NAME/releases/download/vVERSION/.
func upgradeServer(t *testing.T, files map[string][]byte) *upgradeSource {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/oliveagle/git-ai/releases/latest" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"tag_name": "v%s"}`, upgradeTestVersion)
			return
		}
		if body, ok := files[filepath.Base(r.URL.Path)]; ok {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	return &upgradeSource{api: server.URL, files: server.URL, do: server.Client().Do}
}

// releasePackage builds the package of version and the row of its
// checksums.txt, the way the release workflow attaches both to a release.
func releasePackage(t *testing.T, v string) (name string, packageData []byte, checksums string) {
	t.Helper()

	name, _ = upgradePackage(v, "linux", "amd64")
	packageData = tarPackage(t, map[string]string{
		"graft":        "#!/bin/sh\necho graft " + v + "\n",
		"graft-cache":  "#!/bin/sh\necho graft-cache " + v + "\n",
		"graft-verify": "#!/bin/sh\necho graft-verify " + v + "\n",
		"graft-bench":  "#!/bin/sh\necho graft-bench " + v + "\n",
	})
	sum := sha256.Sum256(packageData)
	return name, packageData, fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name)
}

func TestUpgradePackageNames(t *testing.T) {
	t.Parallel()

	name, zipped := upgradePackage("0.7.0", "linux", "amd64")
	require.Equal(t, "git-ai-0.7.0-linux-amd64.tar.gz", name)
	require.False(t, zipped)

	name, zipped = upgradePackage("0.7.0", "darwin", "arm64")
	require.Equal(t, "git-ai-0.7.0-darwin-arm64.tar.gz", name)
	require.False(t, zipped)

	name, zipped = upgradePackage("0.7.0", "windows", "amd64")
	require.Equal(t, "git-ai-0.7.0-windows-amd64.zip", name)
	require.True(t, zipped)
}

func TestUpgradeToolsOfEverySystem(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{"graft", "graft-cache", "graft-verify", "graft-bench"}, upgradeTools("linux"))
	require.Equal(t, []string{"graft.exe", "graft-cache.exe", "graft-verify.exe", "graft-bench.exe"}, upgradeTools("windows"))
}

func TestReleaseOf(t *testing.T) {
	t.Parallel()

	require.Equal(t, "0.7.0", releaseOf("0.7.0"))
	require.Equal(t, "0.7.0", releaseOf("0.7.0-2-g1a2b3c4"))
	require.Equal(t, "unknown", releaseOf("unknown"))
}

func TestParseChecksums(t *testing.T) {
	t.Parallel()

	first := strings.Repeat("1a", 32)
	second := strings.Repeat("9c", 32)
	rows := first + "  git-ai-0.7.0-linux-amd64.tar.gz\n" + second + "  checksums.txt\n"
	checksums, err := parseChecksums(rows)
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"git-ai-0.7.0-linux-amd64.tar.gz": first,
		"checksums.txt":                   second,
	}, checksums)

	_, err = parseChecksums("this line is not a checksum row\n")
	require.Error(t, err)

	_, err = parseChecksums("\n")
	require.Error(t, err)
}

func TestVerifyChecksum(t *testing.T) {
	t.Parallel()

	body := []byte("the package")
	sum := sha256.Sum256(body)
	checksums := map[string]string{"pkg": hex.EncodeToString(sum[:])}

	require.NoError(t, verifyChecksum(checksums, "pkg", body))
	require.Error(t, verifyChecksum(checksums, "pkg", []byte("another package")))
	require.Error(t, verifyChecksum(checksums, "missing", body))
}

func TestUnpackToolsRefusesEntriesOutsideTheTopLevel(t *testing.T) {
	t.Parallel()

	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	archive := tar.NewWriter(gz)
	require.NoError(t, archive.WriteHeader(&tar.Header{Name: "sub/graft", Mode: 0o755, Size: 1}))
	_, err := archive.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, gz.Close())

	dir := t.TempDir()
	_, err = unpackTools(raw.Bytes(), false, dir, upgradeTools("linux"))
	require.Error(t, err)
	require.NoFileExists(t, filepath.Join(dir, "graft"))
}

func TestUpgradeReplacesTheTools(t *testing.T) {
	t.Parallel()

	name, packageData, checksums := releasePackage(t, upgradeTestVersion)
	source := upgradeServer(t, map[string][]byte{name: packageData, "checksums.txt": []byte(checksums)})

	directory := t.TempDir()
	for _, tool := range upgradeTools("linux") {
		require.NoError(t, os.WriteFile(filepath.Join(directory, tool), []byte("# the old tool\n"), 0o755))
	}

	options := defaultOptions()
	var stdout bytes.Buffer
	err := runUpgrade(t.Context(), &stdout, &options, &upgradeFlags{repo: "oliveagle/git-ai"}, source, directory)
	require.NoError(t, err)
	require.Contains(t, stdout.String(), upgradeTestVersion)

	for _, tool := range upgradeTools("linux") {
		installed, err := os.ReadFile(filepath.Join(directory, tool))
		require.NoError(t, err, tool)
		require.Contains(t, string(installed), upgradeTestVersion, tool)
		info, err := os.Stat(filepath.Join(directory, tool))
		require.NoError(t, err, tool)
		require.NotZero(t, info.Mode().Perm()&0o100, "%s keeps its executable bit", tool)
	}
}

func TestUpgradeAsksTheReleaseAPIForTheNewestRelease(t *testing.T) {
	t.Parallel()

	name, packageData, checksums := releasePackage(t, upgradeTestVersion)
	source := upgradeServer(t, map[string][]byte{name: packageData, "checksums.txt": []byte(checksums)})

	directory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(directory, "graft"), []byte("# the old tool\n"), 0o755))

	options := defaultOptions()
	var stdout bytes.Buffer
	err := runUpgrade(t.Context(), &stdout, &options, &upgradeFlags{repo: "oliveagle/git-ai"}, source, directory)
	require.NoError(t, err)
	require.Contains(t, stdout.String(), upgradeTestVersion)
}

func TestUpgradeRefusesAPackageThatFailsItsChecksum(t *testing.T) {
	t.Parallel()

	name, packageData, _ := releasePackage(t, upgradeTestVersion)
	checksums := "0000000000000000000000000000000000000000000000000000000000000000  " + name + "\n"
	source := upgradeServer(t, map[string][]byte{name: packageData, "checksums.txt": []byte(checksums)})

	directory := t.TempDir()
	oldTool := filepath.Join(directory, "graft")
	require.NoError(t, os.WriteFile(oldTool, []byte("# the old tool\n"), 0o755))

	options := defaultOptions()
	err := runUpgrade(t.Context(), &bytes.Buffer{}, &options, &upgradeFlags{repo: "oliveagle/git-ai"}, source, directory)
	require.Error(t, err)
	require.Contains(t, err.Error(), "SHA-256")

	installed, err := os.ReadFile(oldTool)
	require.NoError(t, err)
	require.Equal(t, "# the old tool\n", string(installed), "the old tool survives a failed upgrade")
}

func TestUpgradeCheckInstallsNothing(t *testing.T) {
	t.Parallel()

	name, packageData, checksums := releasePackage(t, upgradeTestVersion)
	source := upgradeServer(t, map[string][]byte{name: packageData, "checksums.txt": []byte(checksums)})

	directory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(directory, "graft"), []byte("# the old tool\n"), 0o755))

	options := defaultOptions()
	var stdout bytes.Buffer
	err := runUpgrade(t.Context(), &stdout, &options, &upgradeFlags{repo: "oliveagle/git-ai", check: true}, source, directory)
	require.NoError(t, err)
	require.Contains(t, stdout.String(), "latest release "+upgradeTestVersion)

	installed, err := os.ReadFile(filepath.Join(directory, "graft"))
	require.NoError(t, err)
	require.Equal(t, "# the old tool\n", string(installed), "--check installs nothing")
}

func TestUpgradeDeclinesToDowngradeADevBuild(t *testing.T) {
	// The test shares the currentVersion variable with the other tests of the
	// command, so it does not run in parallel with them.
	name, packageData, checksums := releasePackage(t, upgradeTestVersion)
	source := upgradeServer(t, map[string][]byte{name: packageData, "checksums.txt": []byte(checksums)})

	previous := currentVersion
	currentVersion = func() string { return upgradeTestVersion + "-2-g1a2b3c4" }
	t.Cleanup(func() { currentVersion = previous })

	options := defaultOptions()
	var stdout bytes.Buffer
	err := runUpgrade(t.Context(), &stdout, &options, &upgradeFlags{repo: "oliveagle/git-ai"}, source, t.TempDir())
	require.NoError(t, err)
	require.Contains(t, stdout.String(), "already on release "+upgradeTestVersion)
}

func TestUpgradeJSONSaysWhatWasReplaced(t *testing.T) {
	t.Parallel()

	name, packageData, checksums := releasePackage(t, upgradeTestVersion)
	source := upgradeServer(t, map[string][]byte{name: packageData, "checksums.txt": []byte(checksums)})

	options := defaultOptions()
	options.jsonOut = true
	var stdout bytes.Buffer
	err := runUpgrade(t.Context(), &stdout, &options, &upgradeFlags{repo: "oliveagle/git-ai"}, source, t.TempDir())
	require.NoError(t, err)
	require.Contains(t, stdout.String(), `"to":"`+upgradeTestVersion+`"`)
	require.Contains(t, stdout.String(), `"replaced":["graft","graft-cache","graft-verify","graft-bench"]`)
}

func TestUpgradeVersionCommandPrintsTheVersion(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr)
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout.String(), "graft, version")
	require.Contains(t, stdout.String(), "OS/Arch:")
}

func TestUpgradeUsageErrors(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run([]string{"upgrade", "bogus"}, &stdout, &stderr)
	require.Equal(t, exitUsage, code)
}

func TestPackageEntry(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		"graft":     "graft",
		"./graft":   "graft",
		`graft.exe`: "graft.exe",
	} {
		got, err := packageEntry(input)
		require.NoError(t, err, input)
		require.Equal(t, want, got, input)
	}

	for _, name := range []string{"", ".", "..", "sub/graft", "../graft", "/graft"} {
		_, err := packageEntry(name)
		require.Error(t, err, name)
	}
}
