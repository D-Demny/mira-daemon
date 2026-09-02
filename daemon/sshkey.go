package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// epic 10 ticket10-3: after the first successful password login the daemon
// installs its own SSH key on the Pi so every later SSH operation can run
// key-first (no password round-trip). The key pair lives on the device side
// at a fixed path (the firmware build creates the directory 0700):
//
//	/etc/mira/ssh/id_ed25519        private key (0600)
//	/etc/mira/ssh/id_ed25519.pub    public key (0644)
//
// Generation is lazy (first use) and idempotent; installation onto the Pi
// happens right after a successful password-authenticated provisioning run
// (see SetupPiService.run) and is idempotent as well (grep -qxF before the
// append). ticket10-4 (auto-reconnect) reuses RunKeyFirst.

// defaultSshKeyPath is where the daemon keeps its device-side key pair.
const defaultSshKeyPath = "/etc/mira/ssh/id_ed25519"

// sshKeyPathEnv overrides the key location (tests, custom images).
const sshKeyPathEnv = "MIRA_SSH_KEY_PATH"

// sshKeyGenTimeout bounds a single ssh-keygen call: local file I/O only,
// anything slower is hung.
const sshKeyGenTimeout = 10 * time.Second

// sshConnectTimeout is the per-attempt connection timeout for both the key
// and the password attempt.
const sshConnectTimeout = 10

// KeyPath resolves the device-side key location: env override (tests) wins
// over the default rootfs location.
func KeyPath() string {
	if p := os.Getenv(sshKeyPathEnv); p != "" {
		return p
	}
	return defaultSshKeyPath
}

// EnsureKey lazily generates the ed25519 key pair. It is a no-op when both
// the private key and its .pub already exist, so restarts and re-runs never
// rewrite a working pair. A half pair is repaired: a missing .pub is
// derived from the private key, a missing private key (with or without a
// stale .pub) is regenerated from scratch.
func EnsureKey(ctx context.Context) error {
	path := KeyPath()
	_, privErr := os.Stat(path)
	_, pubErr := os.Stat(path + ".pub")
	switch {
	case privErr == nil && pubErr == nil:
		return nil // complete pair - nothing to do
	case privErr == nil:
		return derivePublicKey(ctx, path)
	}

	// private key missing (with or without a stale .pub): (re)generate
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating key directory: %w", err)
	}
	if pubErr == nil {
		_ = os.Remove(path + ".pub") // stale half pair - regenerate both
	}
	gctx, cancel := context.WithTimeout(ctx, sshKeyGenTimeout)
	defer cancel()
	args := []string{"-q", "-t", "ed25519", "-N", "", "-C", "mira@device", "-f", path}
	if out, err := exec.CommandContext(gctx, "ssh-keygen", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh-keygen failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	// ssh-keygen leaves the private key at 0600 already; tighten both
	_ = os.Chmod(path, 0o600)
	_ = os.Chmod(path+".pub", 0o644)
	return nil
}

// derivePublicKey recreates a missing .pub from the existing private key.
func derivePublicKey(ctx context.Context, path string) error {
	gctx, cancel := context.WithTimeout(ctx, sshKeyGenTimeout)
	defer cancel()
	out, err := exec.CommandContext(gctx, "ssh-keygen", "-y", "-f", path).Output()
	if err != nil {
		return fmt.Errorf("deriving public key: %v", err)
	}
	if err := os.WriteFile(path+".pub", out, 0o644); err != nil {
		return fmt.Errorf("writing public key: %w", err)
	}
	return nil
}

// ReadPublicKey returns the single-line public key at path+".pub".
func ReadPublicKey(path string) (string, error) {
	b, err := os.ReadFile(path + ".pub")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// sshTransportError reports whether an ssh failure happened at the
// connection/authentication level (ssh exit 255) rather than being the
// remote command's own exit code.
func sshTransportError(err error) bool {
	if err == nil {
		return false
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode() == 255
	}
	// spawn/lookup errors (binary missing, exec format) are not a successful
	// key attempt either - fall back to the password attempt
	return true
}

// runSshKey runs command on user@host authenticated with the device key
// only: BatchMode (no password prompt, fails fast), no inherited agent
// (SSH_AUTH_SOCK emptied, so a foreign key cannot mask whether OUR key
// works), accept-new known-host handling.
func runSshKey(ctx context.Context, host, user, command string) (string, error) {
	args := []string{
		"-i", KeyPath(),
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", fmt.Sprintf("ConnectTimeout=%d", sshConnectTimeout),
		"-o", "LogLevel=ERROR",
		user + "@" + host,
		command,
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runSshPass runs command on user@host with the password via sshpass; the
// password travels only in the SSHPASS environment, never on the command
// line (same contract as setup-pi.sh).
func runSshPass(ctx context.Context, host, user, password, command string) (string, error) {
	args := []string{
		"-e",
		"ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-o", "LogLevel=ERROR",
		user + "@" + host,
		command,
	}
	cmd := exec.CommandContext(ctx, "sshpass", args...)
	cmd.Env = append(os.Environ(), "SSHPASS="+password)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// RunKeyFirst runs one command on the Pi, key-auth first, password
// fallback. It returns which authentication served the successful run.
//
// Only a connection/authentication failure of the key attempt (ssh exit
// 255) may fall back to the password: any other non-zero exit code is the
// remote command's own result (it already ran) and is returned as-is - a
// password retry would double-execute it.
func RunKeyFirst(ctx context.Context, host, user, password, command string) (usedKey bool, output string, err error) {
	out, keyErr := runSshKey(ctx, host, user, command)
	if keyErr == nil {
		return true, out, nil
	}
	if !sshTransportError(keyErr) {
		// the key attempt authenticated and the remote command failed -
		// a password retry would double-execute it
		return false, out, fmt.Errorf("ssh key attempt: %v", keyErr)
	}
	passOut, passErr := runSshPass(ctx, host, user, password, command)
	if passErr == nil {
		return false, passOut, nil
	}
	return false, passOut, fmt.Errorf("ssh to %s@%s failed: key: %v; password: %v", user, host, keyErr, passErr)
}

// installKeyScript is the remote program that puts the device public key
// into the Pi's authorized_keys. Idempotent: grep -qxF skips the append
// when the line already exists (re-runs, re-provisioning). The leading
// MIRA_KEY_INSTALL marker doubles as the fixture hook in the daemon tests.
const installKeyScript = `# MIRA_KEY_INSTALL
mkdir -p "$HOME/.ssh"
chmod 700 "$HOME/.ssh"
AK="$HOME/.ssh/authorized_keys"
touch "$AK"
LINE="__MIRA_PUBKEY__"
if grep -qxF "$LINE" "$AK" 2>/dev/null; then
    echo "mira-key: already present"
else
    printf '%s\n' "$LINE" >> "$AK"
    echo "mira-key: installed"
fi
chmod 600 "$AK"`

// installKeyCommand renders the remote key-install program for the given
// public key. The key is embedded in double quotes, so it must not contain
// characters that would break the quoting; the daemon-generated key
// (ssh-ed25519 <base64> mira@device) never does, and anything else is
// refused instead of risking a mangled command.
func installKeyCommand(publicKey string) (string, error) {
	for _, bad := range []string{"\"", "$", "`", "\n"} {
		if strings.Contains(publicKey, bad) {
			return "", fmt.Errorf("public key contains unsupported character %q", bad)
		}
	}
	if strings.TrimSpace(publicKey) == "" {
		return "", errors.New("public key is empty")
	}
	return strings.Replace(installKeyScript, "__MIRA_PUBKEY__", publicKey, 1), nil
}
