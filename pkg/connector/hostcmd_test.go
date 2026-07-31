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
func TestResolveSrcWalksUpFromBinary(t *testing.T) {
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
		if got := resolveSrc(bin); got != src {
			t.Errorf("resolveSrc(%s) = %q, want %q", bin, got, src)
		}
	}
}

// $CORTEN_SRC wins even when the binary sits nowhere near a checkout — the case
// for an installed binary copied out of the tree (e.g. to the LaunchAgent path).
func TestResolveSrcPrefersEnvVar(t *testing.T) {
	src := mkCheckout(t, cortenModulePath, false)
	t.Setenv(cortenSrcEnv, src)

	stray := filepath.Join(t.TempDir(), "corten-matrix")
	if strings.ContainsAny(stray, " \t") {
		t.Skipf("temp dir %q contains spaces", stray)
	}
	if got := resolveSrc(stray); got != src {
		t.Errorf("resolveSrc(%s) = %q, want %q", stray, got, src)
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
