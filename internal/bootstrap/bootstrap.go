// Package bootstrap provides the lifecycle operations of the bare
// binary: first-run auto-provisioning, the --init turnkey installer
// and the --purge destructive reset. It embeds the default filesystem
// skeleton (skel/) and the systemd unit.
package bootstrap

import (
	"bufio"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ostap-mykhaylyak/rukh/internal/paths"
)

//go:embed all:skel
var skelFS embed.FS

//go:embed rukh.service
var UnitFile []byte

// skel source paths inside the embedded FS.
const (
	skelConfig    = "skel/etc/rukh/config.yaml"
	skelHints     = "skel/etc/rukh/hints"
	skelLogrotate = "skel/etc/logrotate.d/rukh"
)

// EnsureLayout creates the default filesystem layout and installs the
// default config WITHOUT overwriting an existing one. Used both by
// --init and by the first daemon start without a config.
func EnsureLayout(out io.Writer) error {
	for _, dir := range []string{paths.ConfigDir, paths.HintsDir, paths.LogDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	created, err := installIfMissing(skelConfig, paths.ConfigFile, 0o640)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(out, "rukh: installed default config at %s\n", paths.ConfigFile)
	}
	// The documented example of a manual hints file. Its name ends in
	// .example, so the loader ignores it until it is copied.
	entries, err := skelFS.ReadDir(skelHints)
	if err != nil {
		return fmt.Errorf("embedded skel: %w", err)
	}
	for _, e := range entries {
		dst := filepath.Join(paths.HintsDir, e.Name())
		if _, err := installIfMissing(skelHints+"/"+e.Name(), dst, 0o640); err != nil {
			return err
		}
	}
	return nil
}

// installIfMissing copies an embedded skel file to dst unless dst
// already exists (operator files are never overwritten).
func installIfMissing(src, dst string, perm fs.FileMode) (bool, error) {
	if _, err := os.Stat(dst); err == nil {
		return false, nil
	}
	data, err := skelFS.ReadFile(src)
	if err != nil {
		return false, fmt.Errorf("embedded skel: %w", err)
	}
	if err := os.WriteFile(dst, data, perm); err != nil {
		return false, fmt.Errorf("install %s: %w", dst, err)
	}
	return true, nil
}

// Init is the turnkey installer behind --init: layout, binary in
// /usr/sbin, systemd unit, logrotate policy. Lifecycle mode: it acts
// on the filesystem and does NOT assume a running service. Running it
// again over an existing installation is the upgrade path: the config
// is never overwritten.
func Init(version string, out io.Writer) error {
	if err := requireRootLinux("--init"); err != nil {
		return err
	}
	if err := EnsureLayout(out); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	if self != paths.Binary {
		if err := copyFile(self, paths.Binary, 0o755); err != nil {
			return fmt.Errorf("install binary: %w", err)
		}
		fmt.Fprintf(out, "rukh: installed binary at %s\n", paths.Binary)
	}

	if err := os.WriteFile(paths.UnitFile, UnitFile, 0o644); err != nil {
		return fmt.Errorf("install systemd unit: %w", err)
	}
	fmt.Fprintf(out, "rukh: installed systemd unit at %s\n", paths.UnitFile)

	data, err := skelFS.ReadFile(skelLogrotate)
	if err != nil {
		return fmt.Errorf("embedded skel: %w", err)
	}
	if err := os.WriteFile(paths.LogrotateFile, data, 0o644); err != nil {
		return fmt.Errorf("install logrotate policy: %w", err)
	}
	fmt.Fprintf(out, "rukh: installed logrotate policy at %s\n", paths.LogrotateFile)

	fmt.Fprintf(out, nextSteps, version, paths.ConfigFile, paths.HintsDir, paths.NginxConf)
	return nil
}

// nextSteps is the post-install guide. It is a package-level constant
// so a test can check it keeps using the documented command forms:
// the service verbs are bare words, only the rest takes a leading --.
const nextSteps = `
rukh %s installed. Next steps:

  1. move nginx off the public ports, keeping everything else as it is:
       in every server block replace
         listen 80;            with  listen 127.0.0.1:8080;
         listen 443 ssl;       with  listen 127.0.0.1:8443 ssl;
       (or use a single plain 127.0.0.1:8080 block: rukh terminates TLS)
       note: the PORT must change — 127.0.0.1:80 still collides with
       the wildcard :80 rukh binds by default
     and let nginx trust the local proxy:
         set_real_ip_from 127.0.0.1;
         real_ip_header X-Forwarded-For;
       then: nginx -t && systemctl reload nginx
  2. review %s (defaults are meant to be enough)
     behind a CDN, list each site's static resources in
     %s/<hostname>.yaml: they never reach this server, so
     rukh cannot learn them by watching traffic
  3. systemctl daemon-reload
  4. systemctl enable --now rukh
  5. rukh status

rukh reads %s to discover the virtual hosts and their certificates:
nothing about certificates has to be configured twice.
`

// PurgeTargets returns, in one place, everything the app creates at
// runtime. The purge stays automatically aligned with the layout.
func PurgeTargets() []string {
	return []string{paths.ConfigDir, paths.LogDir, paths.RunDir}
}

// allowedPurgePrefixes guards against a misconfigured paths package in
// a custom build: purge refuses to touch anything outside these.
var allowedPurgePrefixes = []string{"/etc/rukh", "/var/log/rukh", "/run/rukh"}

// Purge is the destructive reset behind --purge: removes ALL config,
// data and logs, and finally the binary itself, returning the host to
// "never installed".
func Purge(assumeYes bool, in io.Reader, out io.Writer) error {
	if err := requireRootLinux("--purge"); err != nil {
		return err
	}

	// Never delete data under a live process.
	if err := exec.Command("systemctl", "is-active", "--quiet", "rukh.service").Run(); err == nil {
		return fmt.Errorf("service is running: stop it first (systemctl stop rukh)")
	}

	targets := PurgeTargets()
	for _, t := range targets {
		if !purgeAllowed(t) {
			return fmt.Errorf("refusing to remove unexpected path %q", t)
		}
	}

	fmt.Fprintln(out, "The following paths and ALL their contents will be removed:")
	for _, t := range targets {
		fmt.Fprintln(out, "  ", t)
	}
	fmt.Fprintln(out, "  ", paths.UnitFile)
	fmt.Fprintln(out, "  ", paths.LogrotateFile)
	fmt.Fprintln(out, "  ", paths.Binary)
	fmt.Fprintln(out, "(the nginx configuration is never touched)")
	if !assumeYes {
		if !stdinIsTerminal(in) {
			return fmt.Errorf("refusing to purge without --yes (stdin is not a terminal)")
		}
		fmt.Fprint(out, "Type 'yes' to confirm: ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			return fmt.Errorf("aborted")
		}
	}

	var errs []string
	removed := 0
	// The binary goes last: it is the one running this code.
	all := append(targets, paths.UnitFile, paths.LogrotateFile, paths.Binary)
	for _, t := range all {
		if _, err := os.Stat(t); os.IsNotExist(err) {
			continue
		}
		if err := os.RemoveAll(t); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		fmt.Fprintln(out, "removed", t)
		removed++
	}
	fmt.Fprintf(out, "removed %d path(s)\n", removed)
	fmt.Fprintln(out, "remember to put nginx back on ports 80/443 if you are done with rukh")
	if len(errs) > 0 {
		return fmt.Errorf("some paths could not be removed: %s", strings.Join(errs, "; "))
	}
	return nil
}

func purgeAllowed(path string) bool {
	if path == "" || path == "/" {
		return false
	}
	for _, p := range allowedPurgePrefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

func requireRootLinux(op string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("%s only runs on Linux", op)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("%s requires root", op)
	}
	return nil
}

func stdinIsTerminal(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func copyFile(src, dst string, perm fs.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	// Write to a temp file in the same dir and rename: atomic, and it
	// works even while the destination is being executed (ETXTBSY).
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
