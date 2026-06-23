// corten-matrix - host-side management CLI.
//
// These subcommands give bridge users the same familiar operations as the old
// Makefile / Docker `imessage` wrapper, but baked into the binary: setup,
// setup-beeper, start, stop, restart, status, logs, bbctl, reset, uninstall.
//
// Design:
//   - setup / setup-beeper / reset run the project's existing shell scripts,
//     embedded into the binary (so behaviour matches what users know today).
//   - start / stop / restart / status / logs / bbctl are handled natively
//     (launchd on macOS, systemd --user on Linux).
//   - Docker-aware: inside a container, host-lifecycle commands no-op because
//     Docker Compose + the `imessage` host wrapper drive the lifecycle there;
//     the bridge daemon itself still runs via the container entrypoint.

// Package cli is the shared, CGO-free management/install CLI used by both the
// pure-Go installer bundle (cmd/corten-installer) and the post-install bridge
// binary (cmd/corten-matrix).
package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/lrhodin/corten-matrix/pkg/bbctl"
	"github.com/lrhodin/corten-matrix/scripts"
)

const cortenBundleID = "com.lrhodin.corten-matrix"

func cortenDataDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "corten-matrix")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "corten-matrix")
}

// DataDir returns the bridge data directory (~/.local/share/corten-matrix, or
// $XDG_DATA_HOME/corten-matrix) where config.yaml lives. Exported so the login
// subcommand resolves the same path as the rest of the CLI.
func DataDir() string { return cortenDataDir() }

// selfPath returns the absolute path of this binary (resolving symlinks).
func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		return rp
	}
	return p
}

func streamRun(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// exitWith runs a command, streaming stdio, and exits with its status code.
func exitWith(name string, args ...string) {
	if err := streamRun(name, args...); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "corten-matrix: %s: %v\n", name, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// runEmbeddedScript extracts an embedded management script to a temp file and
// execs it with args, streaming stdio and propagating its exit code.
func runEmbeddedScript(name string, args ...string) {
	data, err := scripts.Files.ReadFile(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "corten-matrix: embedded script %q missing: %v\n", name, err)
		os.Exit(1)
	}
	tmp, err := os.CreateTemp("", "corten-*.sh")
	if err != nil {
		fmt.Fprintf(os.Stderr, "corten-matrix: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		fmt.Fprintf(os.Stderr, "corten-matrix: %v\n", err)
		os.Exit(1)
	}
	tmp.Close()
	_ = os.Chmod(tmp.Name(), 0o755)
	exitWith("/bin/bash", append([]string{tmp.Name()}, args...)...)
}

// serviceCtl controls the installed bridge service.
func serviceCtl(action string) {
	if runtime.GOOS == "darwin" {
		plist := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", cortenBundleID+".plist")
		switch action {
		case "start":
			exitWith("launchctl", "load", "-w", plist)
		case "stop":
			exitWith("launchctl", "unload", "-w", plist)
		case "restart":
			exitWith("launchctl", "kickstart", "-k", "gui/"+strconv.Itoa(os.Getuid())+"/"+cortenBundleID)
		case "status":
			exitWith("launchctl", "list", cortenBundleID)
		}
		return
	}
	// Linux: systemd --user unit installed by install-linux.sh.
	const unit = "corten-matrix.service"
	switch action {
	case "start":
		exitWith("systemctl", "--user", "start", unit)
	case "stop":
		exitWith("systemctl", "--user", "stop", unit)
	case "restart":
		exitWith("systemctl", "--user", "restart", unit)
	case "status":
		exitWith("systemctl", "--user", "status", unit)
	}
}

// offerAddToPath offers to symlink the binary into /usr/local/bin so `corten-matrix`
// works as a bare command. No shell-rc edits — just a symlink in a standard PATH dir.
func offerAddToPath(self string) {
	const target = "/usr/local/bin/corten-matrix"
	if self == target {
		return
	}
	if p, err := exec.LookPath("corten-matrix"); err == nil && p != "" {
		return // already on PATH
	}
	fmt.Printf("  Add 'corten-matrix' to your PATH (symlink %s)? [Y/n]: ", target)
	var ans string
	fmt.Scanln(&ans)
	if ans == "n" || ans == "N" || ans == "no" || ans == "No" {
		return
	}
	_ = os.Remove(target)
	if os.Symlink(self, target) == nil {
		fmt.Printf("  %s✓%s corten-matrix is on your PATH\n", cGreen, cReset)
		return
	}
	c := exec.Command("sudo", "ln", "-sf", self, target)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if c.Run() == nil {
		fmt.Printf("  %s✓%s corten-matrix is on your PATH\n", cGreen, cReset)
		return
	}
	fmt.Printf("  run manually: sudo ln -sf %s %s\n", self, target)
}

// serviceInstall installs (and starts) the bridge as a user service pointing at
// this binary. setup/setup-beeper call this; it's also exposed as a manual command.
func serviceInstall() {
	self := selfPath()
	offerAddToPath(self)
	data := cortenDataDir()
	_ = os.MkdirAll(filepath.Join(data, "logs"), 0o755)
	if runtime.GOOS == "darwin" {
		dir := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents")
		_ = os.MkdirAll(dir, 0o755)
		plist := filepath.Join(dir, cortenBundleID+".plist")
		body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array><string>%s</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>WorkingDirectory</key><string>%s</string>
  <key>StandardOutPath</key><string>%s/logs/bridge.log</string>
  <key>StandardErrorPath</key><string>%s/logs/bridge.log</string>
</dict></plist>
`, cortenBundleID, self, data, data, data)
		if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
			die("write launchd plist: %v", err)
		}
		_ = exec.Command("launchctl", "unload", plist).Run()
		exitWith("launchctl", "load", "-w", plist)
	}
	// Linux: systemd --user unit.
	dir := filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user")
	_ = os.MkdirAll(dir, 0o755)
	unit := filepath.Join(dir, "corten-matrix.service")
	body := fmt.Sprintf(`[Unit]
Description=corten-matrix iMessage bridge
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s
WorkingDirectory=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, self, data)
	if err := os.WriteFile(unit, []byte(body), 0o644); err != nil {
		die("write systemd unit: %v", err)
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	exitWith("systemctl", "--user", "enable", "--now", "corten-matrix.service")
}

// serviceUninstall stops and removes the bridge service unit.
func serviceUninstall() {
	if runtime.GOOS == "darwin" {
		plist := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", cortenBundleID+".plist")
		_ = exec.Command("launchctl", "unload", "-w", plist).Run()
		_ = os.Remove(plist)
		fmt.Println("corten-matrix service removed.")
		os.Exit(0)
	}
	_ = exec.Command("systemctl", "--user", "disable", "--now", "corten-matrix.service").Run()
	_ = os.Remove(filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user", "corten-matrix.service"))
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Println("corten-matrix service removed.")
	os.Exit(0)
}

func tailLogs() {
	exitWith("tail", "-F", filepath.Join(cortenDataDir(), "logs", "bridge.log"))
}

func runBbctl(args []string) {
	// bbctl is compiled into this binary (pkg/bbctl) — run it in-process,
	// no separate bbctl binary to ship or locate.
	bbctl.Run(append([]string{"bbctl"}, args...))
	os.Exit(0)
}

// printHelp shows the user-facing command list (clean, accent-colored).
func PrintHelp() {
	hdr := cBold + cAccent + "◆ corten-matrix" + cReset
	fmt.Println()
	fmt.Printf("  %s  %sMatrix ↔ iMessage bridge%s\n\n", hdr, cDim, cReset)
	fmt.Printf("  %sUsage:%s corten-matrix <command>\n\n", cDim, cReset)
	rows := [][2]string{
		{"setup", "configure & start the bridge"},
		{"setup-beeper", "configure for Beeper"},
		{"start", "start the bridge"},
		{"stop", "stop the bridge"},
		{"restart", "restart the bridge"},
		{"status", "show service status"},
		{"logs", "tail the bridge log"},
		{"install-service", "install + start the background service"},
		{"uninstall-service", "stop + remove the background service"},
		{"reset", "reset bridge state"},
		{"uninstall", "remove the service"},
		{"login", "re-run the iMessage login flow"},
		{"bbctl <args>", "Beeper bridge-manager CLI"},
		{"help", "show this help"},
	}
	for _, r := range rows {
		fmt.Printf("    %s%-14s%s %s%s%s\n", cAccent, r[0], cReset, cDim, r[1], cReset)
	}
	fmt.Println()
}

// isManagementCommand reports whether cmd is a host-management subcommand.
func IsManagementCommand(cmd string) bool {
	switch cmd {
	case "setup", "setup-beeper", "start", "stop", "restart",
		"status", "logs", "bbctl", "reset", "uninstall",
		"install-service", "uninstall-service":
		return true
	}
	return false
}

// runManagementCommand dispatches a host-management subcommand. It always
// terminates the process (os.Exit) — it is only called for known commands.
func RunManagement(cmd string, args []string) {
	self := selfPath()
	dataDir := cortenDataDir()
	bbctl := filepath.Join(filepath.Dir(self), "bbctl")

	switch cmd {
	case "setup":
		if runtime.GOOS == "darwin" {
			runEmbeddedScript("install.sh", self, dataDir, cortenBundleID)
		}
		runEmbeddedScript("install-linux.sh", self, dataDir)
	case "setup-beeper":
		if runtime.GOOS == "darwin" {
			runEmbeddedScript("install-beeper.sh", self, dataDir, cortenBundleID, bbctl)
		}
		runEmbeddedScript("install-beeper-linux.sh", self, dataDir, bbctl)
	case "reset":
		if runtime.GOOS == "darwin" {
			runEmbeddedScript("reset-bridge.sh", cortenBundleID)
		}
		runEmbeddedScript("reset-bridge.sh")
	case "install-service":
		serviceInstall()
	case "uninstall-service", "uninstall":
		serviceUninstall()
	case "start", "stop", "restart", "status":
		serviceCtl(cmd)
	case "logs":
		tailLogs()
	case "bbctl":
		runBbctl(args)
	}
	os.Exit(0)
}
