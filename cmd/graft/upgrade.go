package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/feder-cr/git-ai/internal/version"
)

// upgradeSynopsis is the command line of graft upgrade.
const upgradeSynopsis = "usage: graft upgrade [--check] [--version V] [--repo OWNER/NAME] [--json]"

// upgradeUsage is what graft upgrade prints for a wrong command line.
const upgradeUsage = "upgrade takes no arguments: " + upgradeSynopsis

// upgradeHelp explains graft upgrade below its synopsis.
const upgradeHelp = `Downloads a release of the git-ai tools from GitHub, checks its SHA-256
against the checksums file of that release, and replaces the tools that sit
next to the running graft: graft itself, graft-cache, graft-verify and
graft-bench. The gitaly server is not part of a release; upgrade it by
building it from source.

Without --version the release API names the newest release. --check reports
the current and the newest release and installs nothing. A build that was made
between releases carries the describe suffix of its commit, such as
0.7.0-2-g1a2b3c4: upgrade treats it as already on the 0.7.0 release and
declines to downgrade it to that release, because the release is older than
the commit it was built from.

The replacement of a tool is atomic: the new file is written into the
directory of the running tool and renamed over it. On Windows the running
graft.exe cannot be truncated, so it is renamed to graft.exe.old first and the
new one takes its place; the .old file is removed when the lock on it is
released, and is left behind otherwise.

GITHUB_TOKEN, when set, is sent to the release API, which raises the rate
limit of an anonymous client. Exit status: 0 when the tools are on the release
this command was asked for, 1 when a step failed, 2 when the command line is
wrong.`

// upgradeRepo is the repository whose releases graft upgrade installs when
// --repo does not name one.
const upgradeRepo = "oliveagle/git-ai"

// upgradeFlags holds the flags of graft upgrade.
type upgradeFlags struct {
	repo    string
	version string
	check   bool
}

// upgradeResult is the JSON output of graft upgrade.
type upgradeResult struct {
	// From is the version the tools had before the upgrade.
	From string `json:"from"`
	// To is the release the tools are on after the upgrade.
	To string `json:"to"`
	// Directory is the directory the tools were replaced in.
	Directory string `json:"directory"`
	// Replaced lists the tools that were replaced.
	Replaced []string `json:"replaced"`
	// UpToDate says that the tools were already on the requested release.
	UpToDate bool `json:"up_to_date"`
}

// upgradeCommand returns the upgrade subcommand of graft.
func upgradeCommand() command {
	f := &upgradeFlags{}
	return command{
		bind: func(fs *flag.FlagSet, o *options) {
			fs.Usage = func() {
				fmt.Fprintf(fs.Output(), "%s\n\n%s\n\nflags:\n", upgradeSynopsis, upgradeHelp)
				fs.PrintDefaults()
			}
			fs.StringVar(&f.repo, "repo", upgradeRepo, "repository whose releases are installed")
			fs.StringVar(&f.version, "version", "", "release to install instead of the newest one, such as 0.7.0")
			fs.BoolVar(&f.check, "check", false, "report the current and the newest release, and install nothing")
			fs.BoolVar(&o.jsonOut, "json", false, "print the result as one JSON object")
		},
		run: func(ctx context.Context, out io.Writer, o *options, args []string) error {
			return cmdUpgrade(ctx, out, o, f, args)
		},
	}
}

// upgradeSource is where releases come from: the API that names releases and
// the host the packages are downloaded from. Tests point both at a local
// server.
type upgradeSource struct {
	api   string
	files string
	do    func(*http.Request) (*http.Response, error)
}

// defaultUpgradeSource returns the release source of GitHub.
func defaultUpgradeSource() *upgradeSource {
	client := &http.Client{}
	return &upgradeSource{
		api:   "https://api.github.com",
		files: "https://github.com",
		do:    client.Do,
	}
}

// releaseVersion returns the version of one release: "latest" asks for the
// newest release, and a tag such as v0.7.0 asks for that release.
func (s *upgradeSource) releaseVersion(ctx context.Context, repo, which string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.api+"/repos/"+repo+"/"+which, nil)
	if err != nil {
		return "", fmt.Errorf("release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.do(req)
	if err != nil {
		return "", fmt.Errorf("release request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release request: %s answered %s", s.api+"/repos/"+repo+"/"+which, resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("release of %s: %w", repo, err)
	}
	if body.TagName == "" {
		return "", fmt.Errorf("release of %s answers no tag", repo)
	}
	return strings.TrimPrefix(body.TagName, "v"), nil
}

// fetch downloads one file of a release.
func (s *upgradeSource) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	resp, err := s.do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: %s answered %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	return body, nil
}

// currentVersion is the version the running graft was built as, "unknown"
// when the build recorded none. It is a variable so that the tests can name a
// version.
var currentVersion = func() string {
	if v := version.GetVersion(); v != "" {
		return v
	}
	return "unknown"
}

// releaseOf returns the release a version describes. A build between releases
// carries the describe suffix of its commit, such as 0.7.0-2-g1a2b3c4; its
// release is the one the suffix counts away from.
func releaseOf(v string) string {
	if i := strings.Index(v, "-"); i >= 0 {
		return v[:i]
	}
	return v
}

// upgradePackage returns the name of the release package of goos and goarch,
// and whether it is a zip. The names are the ones the release workflow
// attaches to a release.
func upgradePackage(v, goos, goarch string) (string, bool) {
	if goos == "windows" {
		return fmt.Sprintf("git-ai-%s-%s-%s.zip", v, goos, goarch), true
	}
	return fmt.Sprintf("git-ai-%s-%s-%s.tar.gz", v, goos, goarch), false
}

// upgradeTools returns the tools of a release for goos, in the order the
// release workflow builds them.
func upgradeTools(goos string) []string {
	names := []string{"graft", "graft-cache", "graft-verify", "graft-bench"}
	if goos != "windows" {
		return names
	}
	exe := make([]string, len(names))
	for i, name := range names {
		exe[i] = name + ".exe"
	}
	return exe
}

// parseChecksums reads the checksums.txt of a release, one "sha256  name" row
// per package as sha256sum(1) writes it. The checksum of every row is
// lowercase hex.
func parseChecksums(text string) (map[string]string, error) {
	checksums := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != hex.EncodedLen(sha256.Size) {
			return nil, fmt.Errorf("checksums.txt: malformed row %q", line)
		}
		checksums[strings.TrimPrefix(fields[1], "*")] = fields[0]
	}
	if len(checksums) == 0 {
		return nil, errors.New("checksums.txt: no rows")
	}
	return checksums, nil
}

// verifyChecksum checks the SHA-256 of data against the checksum the
// checksums file records for name.
func verifyChecksum(checksums map[string]string, name string, data []byte) error {
	want, ok := checksums[name]
	if !ok {
		return fmt.Errorf("checksums.txt does not name %s", name)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("the SHA-256 of %s is %s, but checksums.txt says %s", name, got, want)
	}
	return nil
}

// unpackTools writes the tools of a release package into dir and returns
// their names. The package carries the tools at its top level: an entry that
// leaves the top level is a package nobody built, and is refused instead of
// written.
// packageEntry returns the tool name one entry of a release package carries.
// The tarball of a release stores its tools as ./graft, the zip stores them as
// graft, and an entry that carries more than a single name is a package nobody
// built, and is refused instead of unpacked.
func packageEntry(name string) (string, error) {
	name = strings.TrimPrefix(filepath.ToSlash(name), "./")
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return "", fmt.Errorf("the package carries %q, not a tool of the top level", name)
	}
	return name, nil
}

func unpackTools(data []byte, zipped bool, dir string, tools []string) ([]string, error) {
	want := make(map[string]bool, len(tools))
	for _, name := range tools {
		want[name] = true
	}

	var written []string
	write := func(name string, mode os.FileMode, reader io.Reader) error {
		body, err := io.ReadAll(reader)
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		if mode == 0 {
			mode = 0o755
		}
		mode &= 0o777
		if mode&0o100 == 0 {
			mode |= 0o100
		}
		dest := filepath.Join(dir, name)
		if err := os.WriteFile(dest, body, mode); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
		written = append(written, name)
		return nil
	}

	if zipped {
		reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("reading the package: %w", err)
		}
		for _, file := range reader.File {
			if file.FileInfo().IsDir() {
				continue
			}
			tool, err := packageEntry(file.Name)
			if err != nil {
				return nil, err
			}
			if !want[tool] {
				continue
			}
			entry, err := file.Open()
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", file.Name, err)
			}
			err = write(tool, file.Mode(), entry)
			_ = entry.Close()
			if err != nil {
				return nil, err
			}
		}
		return written, nil
	}

	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("reading the package: %w", err)
	}
	defer func() { _ = gz.Close() }()
	archive := tar.NewReader(gz)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the package: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		tool, err := packageEntry(header.Name)
		if err != nil {
			return nil, err
		}
		if !want[tool] {
			continue
		}
		if err := write(tool, os.FileMode(header.Mode), archive); err != nil {
			return nil, err
		}
	}
	return written, nil
}

// installTool replaces the tool at dest with the one at src: the new file is
// renamed over the old one, so a caller of the tool sees either version, never
// a half-written one. On Windows the running tool cannot be truncated, so the
// old file is renamed out of the way first.
func installTool(src, dest string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(src, dest)
	}
	old := dest + ".old"
	if err := os.Rename(dest, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("moving %s aside: %w", dest, err)
	}
	if err := os.Rename(src, dest); err != nil {
		return fmt.Errorf("installing %s: %w", dest, err)
	}
	_ = os.Remove(old)
	return nil
}

// cmdUpgrade runs graft upgrade: it validates the command line, names the
// release source and the directory of the running tools, and hands the rest to
// runUpgrade.
func cmdUpgrade(ctx context.Context, out io.Writer, o *options, f *upgradeFlags, args []string) error {
	if len(args) != 0 {
		return usagef("%s", upgradeUsage)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding the directory of the tools: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return runUpgrade(ctx, out, o, f, defaultUpgradeSource(), filepath.Dir(executable))
}

// runUpgrade is the implementation of graft upgrade. The source and the
// directory are parameters so that the tests can point the command at a local
// release server and at a scratch directory.
func runUpgrade(ctx context.Context, out io.Writer, o *options, f *upgradeFlags, source *upgradeSource, directory string) error {
	from := currentVersion()

	to := f.version
	if to != "" {
		to = strings.TrimPrefix(to, "v")
	} else {
		latest, err := source.releaseVersion(ctx, f.repo, "releases/latest")
		if err != nil {
			return err
		}
		to = latest
	}

	if f.check {
		return upgradeCheck(out, o, from, to)
	}
	if releaseOf(from) == to {
		return upgradeUpToDate(out, o, from, to)
	}

	name, zipped := upgradePackage(to, runtime.GOOS, runtime.GOARCH)
	packageData, err := source.fetch(ctx, fmt.Sprintf("%s/%s/releases/download/v%s/%s", source.files, f.repo, to, name))
	if err != nil {
		return err
	}
	checksumData, err := source.fetch(ctx, fmt.Sprintf("%s/%s/releases/download/v%s/checksums.txt", source.files, f.repo, to))
	if err != nil {
		return err
	}
	checksums, err := parseChecksums(string(checksumData))
	if err != nil {
		return err
	}
	if err := verifyChecksum(checksums, name, packageData); err != nil {
		return err
	}

	tools := upgradeTools(runtime.GOOS)
	// The tools are staged in the directory they replace, because a rename is
	// only atomic within one file system: a staging directory under TMPDIR is
	// often another one.
	staging, err := os.MkdirTemp(directory, ".graft-upgrade-")
	if err != nil {
		return fmt.Errorf("staging the tools: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	written, err := unpackTools(packageData, zipped, staging, tools)
	if err != nil {
		return err
	}
	if len(written) != len(tools) {
		return fmt.Errorf("the package carries %s, expected %s", strings.Join(written, ", "), strings.Join(tools, ", "))
	}

	var replaced []string
	for _, tool := range tools {
		if err := installTool(filepath.Join(staging, tool), filepath.Join(directory, tool)); err != nil {
			return fmt.Errorf("replacing %s: %w", tool, err)
		}
		replaced = append(replaced, tool)
	}

	result := upgradeResult{From: from, To: to, Directory: directory, Replaced: replaced}
	if o.jsonOut {
		return json.NewEncoder(out).Encode(result)
	}
	fmt.Fprintf(out, "upgraded graft %s → %s in %s (%s)\n", displayText(from), displayText(to), directory, strings.Join(replaced, ", "))
	return nil
}

// upgradeCheck reports the current and the requested release, and installs
// nothing.
func upgradeCheck(out io.Writer, o *options, from, to string) error {
	if o.jsonOut {
		return json.NewEncoder(out).Encode(upgradeResult{From: from, To: to, UpToDate: releaseOf(from) == to})
	}
	fmt.Fprintf(out, "graft %s\nlatest release %s\n", displayText(from), displayText(to))
	if releaseOf(from) == to {
		fmt.Fprintln(out, "the tools are on this release")
	} else if from == "unknown" {
		fmt.Fprintln(out, "the build records no version; graft upgrade installs the release")
	} else {
		fmt.Fprintf(out, "graft upgrade installs %s\n", displayText(to))
	}
	return nil
}

// upgradeUpToDate reports that the tools are already on the requested
// release.
func upgradeUpToDate(out io.Writer, o *options, from, to string) error {
	if o.jsonOut {
		return json.NewEncoder(out).Encode(upgradeResult{From: from, To: to, UpToDate: true})
	}
	fmt.Fprintf(out, "graft %s is already on release %s\n", displayText(from), displayText(to))
	return nil
}

// cmdVersion prints the version of the running graft, so that a user can tell
// what graft upgrade would replace it with.
func cmdVersion(_ context.Context, out io.Writer, _ *options, args []string) error {
	if len(args) != 0 {
		return usagef("usage: graft version")
	}
	fmt.Fprintln(out, version.GetVersionDetails("graft"))
	return nil
}
