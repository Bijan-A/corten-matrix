// corten-matrix - A Matrix-iMessage puppeting bridge.

package connector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkCheckout builds a directory that looks like a corten-matrix source checkout.
// module is written into go.mod verbatim so callers can test the module guard.
func mkCheckout(t *testing.T, module string, gitAsFile bool) string {
	t.Helper()
	d := t.TempDir()
	// usable() rejects paths with spaces, and it exits the process rather than
	// returning an error — skip rather than kill the test binary on a TMPDIR
	// that happens to contain one.
	if strings.ContainsAny(d, " \t") {
		t.Skipf("temp dir %q contains spaces", d)
	}
	if gitAsFile {
		// A git worktree stores .git as a file, not a directory.
		write(t, filepath.Join(d, ".git"), "gitdir: /elsewhere/.git/worktrees/wt\n")
	} else if err := os.Mkdir(filepath.Join(d, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(d, "Makefile"), "build:\n\techo hi\n")
	write(t, filepath.Join(d, "go.mod"), "module "+module+"\n\ngo 1.25.0\n")
	return d
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIsCheckout(t *testing.T) {
	t.Run("accepts a checkout", func(t *testing.T) {
		if d := mkCheckout(t, cortenModulePath, false); !isCheckout(d) {
			t.Fatalf("isCheckout(%s) = false, want true", d)
		}
	})

	// The module guard is what makes walking upward from the binary safe: an
	// unrelated git repo above the binary must not be mistaken for ours.
	t.Run("rejects a foreign module", func(t *testing.T) {
		if d := mkCheckout(t, "github.com/someone/unrelated", false); isCheckout(d) {
			t.Fatal("accepted a repo declaring a different module")
		}
	})

	t.Run("accepts a git worktree", func(t *testing.T) {
		if d := mkCheckout(t, cortenModulePath, true); !isCheckout(d) {
			t.Fatal("rejected a worktree (.git as a file)")
		}
	})

	t.Run("rejects incomplete checkouts", func(t *testing.T) {
		for _, missing := range []string{".git", "Makefile", "go.mod"} {
			d := mkCheckout(t, cortenModulePath, false)
			if err := os.RemoveAll(filepath.Join(d, missing)); err != nil {
				t.Fatal(err)
			}
			if isCheckout(d) {
				t.Errorf("accepted a checkout with no %s", missing)
			}
		}
	})

	t.Run("rejects an empty dir", func(t *testing.T) {
		if isCheckout(t.TempDir()) {
			t.Fatal("accepted an empty directory")
		}
	})
}

// The primary discovery path: `make` leaves the binary in the checkout root, so
// resolving from the binary's own path must find the checkout by walking up.
func TestFindCheckoutWalksUpFromBinary(t *testing.T) {
	t.Setenv(cortenSrcEnv, "")
	src := mkCheckout(t, cortenModulePath, false)

	nested := filepath.Join(src, "pkg", "connector")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, bin := range []string{
		filepath.Join(src, "corten-matrix"),    // built in place by `make`
		filepath.Join(nested, "corten-matrix"), // somewhere deeper in the tree
	} {
		got, _, ok := findCheckout(bin)
		if !ok || got != src {
			t.Errorf("findCheckout(%s) = %q, %v; want %q, true", bin, got, ok, src)
		}
	}
}

// $CORTEN_SRC wins even when the binary sits nowhere near a checkout — the case
// for an installed binary copied out of the tree (e.g. to the LaunchAgent path).
func TestFindCheckoutPrefersEnvVar(t *testing.T) {
	src := mkCheckout(t, cortenModulePath, false)
	t.Setenv(cortenSrcEnv, src)

	stray := filepath.Join(t.TempDir(), "corten-matrix")
	if strings.ContainsAny(stray, " 	") {
		t.Skipf("temp dir %q contains spaces", stray)
	}
	got, _, ok := findCheckout(stray)
	if !ok || got != src {
		t.Errorf("findCheckout(%s) = %q, %v; want %q, true", stray, got, ok, src)
	}
}

// Reporting "not found" instead of exiting is what lets `update` fall back to
// release mode for someone who downloaded a binary and has no checkout.
func TestFindCheckoutReportsMissingWithoutExiting(t *testing.T) {
	t.Setenv(cortenSrcEnv, "")
	// A bare temp dir with no checkout anywhere above it that declares our module.
	stray := filepath.Join(t.TempDir(), "corten-matrix")
	got, tried, ok := findCheckout(stray)
	if ok {
		t.Fatalf("found a checkout at %q where none exists", got)
	}
	if len(tried) == 0 {
		t.Error("no directories reported as tried; the error message would be useless")
	}
}

func TestExtraHostHelpAdvertisesUpdate(t *testing.T) {
	rows := ExtraHostHelp()
	if len(rows) != 1 || rows[0][0] != "update" {
		t.Fatalf("ExtraHostHelp() = %v, want a single 'update' row", rows)
	}
}

// HandleHostCommand must decline everything except `update`, or it would
// swallow the binary's real subcommands.
func TestHandleHostCommandDeclinesOtherCommands(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{},
		{"setup"},
		{"start"},
		{"stop"},
		{"logs", "1"},
		{"bbctl"},
		{"reset", "--yes"},
		{"updated"},     // prefix of "update", must not match
		{"UPDATE"},      // case-sensitive
		{"x", "update"}, // only argv[1] is considered
	} {
		if HandleHostCommand(args, "test", "darwin", "arm64") {
			t.Errorf("HandleHostCommand(%v) claimed the command", args)
		}
	}
}

// ── release mode ────────────────────────────────────────────────────────────

func TestNormalizeVersion(t *testing.T) {
	for in, want := range map[string]string{
		"v1.2.0":    "1.2.0",
		"1.2.0":     "1.2.0",
		"1.2.0\n":   "1.2.0",
		"  v1.2.0 ": "1.2.0",
		"":          "",
	} {
		if got := normalizeVersion(in); got != want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
	// The point of normalising: a `v`-prefixed tag must match a bare version.
	if normalizeVersion("v1.1.0") != normalizeVersion("1.1.0\n") {
		t.Error("v-prefixed tag does not compare equal to bare version")
	}
}

func TestPickAssetPrefersUniversalMacBinary(t *testing.T) {
	rel := &ghRelease{TagName: "v1.2.0", Assets: []ghAsset{
		{Name: "corten-matrix-linux-amd64"},
		{Name: "corten-matrix-darwin-arm64"},
		{Name: "corten-matrix-macos"}, // universal — should win
	}}
	got, err := pickAsset(rel, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "corten-matrix-macos" {
		t.Errorf("picked %q, want corten-matrix-macos", got.Name)
	}
}

func TestPickAssetFallsBackToArchSpecific(t *testing.T) {
	rel := &ghRelease{TagName: "v1.2.0", Assets: []ghAsset{
		{Name: "corten-matrix-darwin-arm64"},
	}}
	got, err := pickAsset(rel, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "corten-matrix-darwin-arm64" {
		t.Errorf("picked %q", got.Name)
	}
}

func TestPickAssetMatchesArchExactly(t *testing.T) {
	// An arm64 host must not be handed the amd64 asset.
	rel := &ghRelease{TagName: "v1.2.0", Assets: []ghAsset{
		{Name: "corten-matrix-linux-amd64"},
	}}
	if _, err := pickAsset(rel, "linux", "arm64"); err == nil {
		t.Fatal("accepted an amd64 asset for an arm64 host")
	}
}

func TestPickAssetErrorsAreInformative(t *testing.T) {
	rel := &ghRelease{TagName: "v1.2.0", Assets: []ghAsset{{Name: "something-else"}}}
	_, err := pickAsset(rel, "darwin", "arm64")
	if err == nil {
		t.Fatal("expected an error")
	}
	// The message must name both what was wanted and what the release has,
	// or the user cannot tell whether the release or the host is wrong.
	for _, want := range []string{"corten-matrix-macos", "something-else", "v1.2.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	if _, err := pickAsset(rel, "windows", "amd64"); err == nil {
		t.Error("expected an error for an unsupported platform")
	}
}

func TestFindChecksum(t *testing.T) {
	asset := ghAsset{Name: "corten-matrix-macos"}
	rel := &ghRelease{Assets: []ghAsset{
		asset,
		{Name: "corten-matrix-macos.sha256", URL: "https://example/sum"},
	}}
	got, ok := findChecksum(rel, asset)
	if !ok || got.URL != "https://example/sum" {
		t.Fatalf("findChecksum() = %+v, %v", got, ok)
	}
	// Absent checksum is not an error — releases predating checksums still work.
	if _, ok := findChecksum(&ghRelease{Assets: []ghAsset{asset}}, asset); ok {
		t.Error("reported a checksum that does not exist")
	}
}

func TestCurrentReleaseRepoEnvOverride(t *testing.T) {
	t.Setenv(releaseRepoEnv, "")
	if got := currentReleaseRepo(); got != releaseRepo {
		t.Errorf("default = %q, want %q", got, releaseRepo)
	}
	t.Setenv(releaseRepoEnv, "someone/elsefork")
	if got := currentReleaseRepo(); got != "someone/elsefork" {
		t.Errorf("override = %q", got)
	}
}

func TestAssetCandidatesCoverPublishedNaming(t *testing.T) {
	// These names are the contract with the release workflow; if they change,
	// downloaded binaries stop being able to update themselves.
	if got := assetCandidates("darwin", "arm64"); got[0] != "corten-matrix-macos" {
		t.Errorf("darwin first candidate = %q", got[0])
	}
	if got := assetCandidates("linux", "amd64"); len(got) != 1 || got[0] != "corten-matrix-linux-amd64" {
		t.Errorf("linux candidates = %v", got)
	}
	if got := assetCandidates("plan9", "amd64"); got != nil {
		t.Errorf("unsupported platform returned %v", got)
	}
}
