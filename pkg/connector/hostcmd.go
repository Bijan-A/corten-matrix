// corten-matrix - A Matrix-iMessage puppeting bridge.

package connector

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// This file is the host-command extension point. Upstream ships it as two
// stubs; the official release build swaps in a private overlay that adds
// `update` (download the latest GitHub release, replace this binary, restart).
//
// A build from source gets neither: the release overlay is not in the public
// tree, so `corten-matrix update` simply does not exist, and there is no repo
// URL, env var, or config key to repoint at a fork. The README says as much —
// "the `update` command isn't included in source builds".
//
// This implements `update` with two modes, because a fork can be consumed both
// ways:
//
//   - source  — rebuild the checkout this binary came from (git pull, make,
//     swap, restart). The only option for a source install, since there is no
//     artifact to fetch.
//   - release — download the latest release asset from the fork and swap it in.
//     For people who downloaded a binary and have no checkout.
//
// Picking between them is automatic: if a checkout is discoverable, rebuild it;
// otherwise fetch a release. Both converge on the same install step, so the
// stop → swap → start behaviour is identical either way.
//
// Everything here is stdlib-only and touches nothing proprietary — no rustpush,
// no open-absinthe, no apple-private-apis, and no part of the closed release
// tooling. See hostcmd_release.go for the download path.

// cortenSrcEnv overrides where the source checkout lives. Everything else is
// discovery; this is the escape hatch when discovery guesses wrong.
const cortenSrcEnv = "CORTEN_SRC"

// cortenModulePath identifies a genuine corten-matrix checkout. A fork keeps the
// module path (Go import paths don't change when you fork), so this recognises
// any fork's checkout while refusing to run `make` in some unrelated repo that
// happens to sit above the binary.
const cortenModulePath = "github.com/lrhodin/corten-matrix"

// homeCandidates are the conventional places to look when the binary isn't
// inside the checkout — e.g. it was copied to the path the LaunchAgent runs.
var homeCandidates = [][]string{
	{"src", "corten-matrix"},
	{"corten-matrix"},
	{"Developer", "corten-matrix"},
	{"git", "corten-matrix"},
	{"Projects", "corten-matrix"},
}

type updateMode int

const (
	modeAuto updateMode = iota
	modeSource
	modeRelease
)

// HandleHostCommand lets the build configuration's host-command extensions
// claim a management subcommand before the binary's normal dispatch. args is
// os.Args[1:]; version/goos/goarch describe the running build. We claim only
// `update` and decline everything else, so normal dispatch is unaffected.
func HandleHostCommand(args []string, version, goos, goarch string) bool {
	if len(args) == 0 || args[0] != "update" {
		return false
	}
	runUpdate(args[1:], version, goos, goarch)
	return true
}

// ExtraHostHelp returns extra {command, description} rows for the `help`
// listing.
func ExtraHostHelp() [][2]string {
	return [][2]string{
		{"update", "Update to the latest version and restart"},
	}
}

func updateUsage() {
	fmt.Println("Usage: corten-matrix update [--source|--release] [--no-pull] [--force] [--check]")
	fmt.Println()
	fmt.Println("  Updates this binary and restarts the bridge.")
	fmt.Println()
	fmt.Println("  With a source checkout present it rebuilds that checkout; without one")
	fmt.Println("  it downloads the latest release. Pick explicitly with --source/--release.")
	fmt.Println()
	fmt.Println("  --source    rebuild from the source checkout (git pull + make)")
	fmt.Println("  --release   download the latest release asset instead of building")
	fmt.Println("  --no-pull   source mode: skip git fetch/pull, build what is checked out")
	fmt.Println("  --force     release mode: reinstall even if already on the latest version")
	fmt.Println("  --check     report what would happen and exit without changing anything")
	fmt.Println()
	fmt.Printf("  $%s   override the checkout location\n", cortenSrcEnv)
	fmt.Printf("  $%s  override the owner/repo releases come from\n", releaseRepoEnv)
}

// runUpdate dispatches to the source or release path. It always terminates the
// process.
func runUpdate(args []string, version, goos, goarch string) {
	mode := modeAuto
	pull, force, check := true, false, false

	for _, a := range args {
		switch a {
		case "--source":
			mode = modeSource
		case "--release":
			mode = modeRelease
		case "--no-pull":
			pull = false
		case "--force":
			force = true
		case "--check":
			check = true
		case "-h", "--help":
			updateUsage()
			os.Exit(0)
		default:
			fatalf("update: unknown argument %q (try --help)", a)
		}
	}

	target := installedPath()

	// Building from source is macOS-only (the Makefile enforces it), but a
	// downloaded release can update itself anywhere, so only gate the source
	// path on the platform.
	src, tried, found := findCheckout(target)
	switch mode {
	case modeSource:
		if !found {
			fatalf("update --source: %s", noCheckoutMessage(tried))
		}
	case modeAuto:
		if found {
			mode = modeSource
		} else {
			mode = modeRelease
		}
	}

	if mode == modeSource {
		if goos != "darwin" {
			fatalf("update --source: building from source is macOS-only (this build reports %s).\n"+
				"Use --release to download a prebuilt binary instead.", goos)
		}
		updateFromSource(src, target, version, pull, check)
		return
	}
	updateFromRelease(target, version, goos, goarch, force, check)
}

func noCheckoutMessage(tried []string) string {
	return fmt.Sprintf("no corten-matrix source checkout found.\n"+
		"Point it at one:\n"+
		"    CORTEN_SRC=/path/to/corten-matrix corten-matrix update --source\n\n"+
		"Looked in:\n  %s", strings.Join(tried, "\n  "))
}

// ── source mode ─────────────────────────────────────────────────────────────

// updateFromSource rebuilds the checkout and swaps the new binary in.
func updateFromSource(src, target, version string, pull, check bool) {
	fmt.Printf("→ mode:      source (rebuild)\n")
	fmt.Printf("→ checkout:  %s\n", src)
	fmt.Printf("→ installed: %s (%s)\n", target, version)

	// A dirty tree means either local experiments or a half-finished rebase.
	// Building it would produce a binary that matches no commit, so refuse
	// rather than quietly shipping it.
	if out := gitOut(src, "status", "--porcelain"); out != "" {
		fatalf("update: %s has uncommitted changes — commit, stash, or discard them first:\n%s", src, out)
	}

	branch := gitOut(src, "rev-parse", "--abbrev-ref", "HEAD")
	origin := gitOut(src, "remote", "get-url", "origin")
	fmt.Printf("→ branch:    %s (origin: %s)\n", branch, origin)

	if check {
		fmt.Println("\n--check: would run `git pull --ff-only` (unless --no-pull), `make`, then stop → swap → start.")
		return
	}

	if pull {
		fmt.Printf("→ pulling %s\n", branch)
		mustRun(src, "git", "fetch", "origin")
		// --ff-only on purpose: if the branch has diverged (typically after a
		// `git rebase upstream/…`), stop and let a human decide instead of
		// merging behind their back. --no-pull is the escape hatch.
		if err := run(src, "git", "pull", "--ff-only"); err != nil {
			fatalf("update: cannot fast-forward %s.\n"+
				"If you rebased onto upstream, that's expected — build what you have with:\n"+
				"    corten-matrix update --source --no-pull", branch)
		}
	}

	fmt.Println("→ building (this takes a while on a cold cargo cache)")
	mustRun(src, "make")

	built := filepath.Join(src, "corten-matrix")
	if _, err := os.Stat(built); err != nil {
		fatalf("update: make finished but %s is missing: %v", built, err)
	}

	swapIn(built, target, version)
}

// ── shared install ──────────────────────────────────────────────────────────

// swapIn stops the bridge, replaces target with newBinary, and starts it again.
// It returns rather than exiting so callers' deferred cleanup (the release
// path's staged download) still runs.
func swapIn(newBinary, target, oldVersion string) {
	// Stop first: the LaunchAgent sets KeepAlive, so launchd would otherwise
	// relaunch the old binary in the middle of the swap.
	fmt.Println("→ stopping bridge")
	mustRun("", target, "stop")

	fmt.Printf("→ installing to %s\n", target)
	if err := installBinary(newBinary, target); err != nil {
		fatalf("update: install failed: %v\n"+
			"The bridge is stopped and the old binary is intact — start it again with:\n"+
			"    corten-matrix start", err)
	}

	fmt.Println("→ starting bridge")
	mustRun("", target, "start")

	fmt.Printf("\n✓ updated %s → %s\n", oldVersion, strings.TrimSpace(cmdOut(target, "--version")))
}

// isCheckout reports whether dir is a corten-matrix source checkout: a git
// repo with a Makefile whose go.mod declares the corten-matrix module. The
// module check is what makes walking up from the binary safe.
func isCheckout(dir string) bool {
	// .git is a directory in a normal clone and a file in a git worktree;
	// Stat accepts both.
	for _, marker := range []string{".git", "Makefile", "go.mod"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err != nil {
			return false
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest) == cortenModulePath
		}
	}
	return false
}

// findCheckout locates the source checkout, reporting failure rather than
// exiting so `update` can fall back to release mode. Order: $CORTEN_SRC, then
// upward from the running binary (covers the common source-build layout, where
// `make` leaves the binary in the checkout root and it is run from there or via
// a symlink), then the conventional home-directory locations. Returns the
// directory, the list of places tried, and whether one was found.
func findCheckout(self string) (string, []string, bool) {
	if dir := os.Getenv(cortenSrcEnv); dir != "" {
		abs := absOrDie(dir)
		if !isCheckout(abs) {
			fatalf("update: $%s=%s is not a corten-matrix checkout\n"+
				"Expected a git repo with a Makefile and go.mod declaring %q.", cortenSrcEnv, abs, cortenModulePath)
		}
		return usable(abs), []string{abs}, true
	}

	var tried []string
	for dir := filepath.Dir(self); ; {
		if isCheckout(dir) {
			return usable(dir), tried, true
		}
		tried = append(tried, dir)
		parent := filepath.Dir(dir)
		if parent == dir { // reached the filesystem root
			break
		}
		dir = parent
	}

	if home, err := os.UserHomeDir(); err == nil {
		for _, parts := range homeCandidates {
			cand := filepath.Join(append([]string{home}, parts...)...)
			if isCheckout(cand) {
				return usable(cand), tried, true
			}
			tried = append(tried, cand)
		}
	}
	return "", tried, false
}

// usable rejects a checkout the build cannot actually use.
func usable(dir string) string {
	// The Makefile hard-errors on spaces (CGO/linker flag expansion); catch it
	// here with a clearer message than a mid-build $(error).
	if strings.ContainsAny(dir, " \t") {
		fatalf("update: checkout path %q contains spaces — CGO and the linker cannot handle that", dir)
	}
	return dir
}

func absOrDie(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		fatalf("update: bad checkout path %q: %v", dir, err)
	}
	return abs
}

// installedPath is the binary this process is running from, with symlinks
// resolved — /usr/local/bin/corten-matrix is a symlink, and the LaunchAgent's
// ProgramArguments points at the resolved target, so that target is what has to
// be replaced. Mirrors pkg/cli.selfPath().
func installedPath() string {
	p, err := os.Executable()
	if err != nil {
		fatalf("update: cannot locate the running binary: %v", err)
	}
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		return rp
	}
	return p
}

// installBinary replaces dst with src atomically. It stages a copy alongside dst
// and renames over it: writing directly to the file backing a running (or
// recently running) executable returns ETXTBSY, and rename never leaves a
// half-written binary at the service's path if we die mid-copy. Copying the
// bytes preserves the ad-hoc signature `make` applied.
func installBinary(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	// Same directory, so the rename stays within one filesystem.
	tmp := filepath.Join(filepath.Dir(dst), ".corten-matrix.new")
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ── small process helpers ───────────────────────────────────────────────────

func run(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func mustRun(dir, name string, args ...string) {
	if err := run(dir, name, args...); err != nil {
		fatalf("update: %s %s failed: %v", name, strings.Join(args, " "), err)
	}
}

// gitOut runs a git command and returns its trimmed stdout, failing hard on error.
func gitOut(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		fatalf("update: git %s failed: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// cmdOut runs a command and returns stdout, tolerating failure (used only for
// the cosmetic version line).
func cmdOut(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "(unknown)"
	}
	return string(out)
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
