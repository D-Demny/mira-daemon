package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// epic 10 ticket10-3: fake-SSH fixtures for the device-side key handling.
// The fakes live in a temp bin dir that is prepended to PATH; the fake ssh
// emulates the Pi's authorized_keys as a local file ($FAKE_PI_AUTHKEYS) and
// reproduces the idempotent append of the MIRA_KEY_INSTALL program.

// fakeSSH is the fake ssh binary.
//
//	key auth      -i present -> succeeds only when FAKE_SSH_KEY_OK=1,
//	             otherwise fails with exit 255 (like a real auth failure)
//	password auth -i absent  (the fake sshpass drops to this mode) ->
//	             succeeds unless FAKE_SSH_PASS_FAIL=1
const fakeSSH = `#!/bin/sh
key_mode=0
for a in "$@"; do
    [ "$a" = "-i" ] && key_mode=1
done
if [ "$key_mode" = 1 ] && [ "${FAKE_SSH_KEY_OK:-0}" != 1 ]; then
    printf 'Permission denied (publickey).\n' >&2
    exit 255
fi
if [ "$key_mode" != 1 ] && [ "${FAKE_SSH_PASS_FAIL:-0}" = 1 ]; then
    printf 'Permission denied (password).\n' >&2
    exit 255
fi
# remote command failure: auth (key) succeeded, the command itself fails.
# Key mode only: a (wrong) fallback into password mode must stay successful.
if [ "$key_mode" = 1 ] && [ "${FAKE_SSH_CMD_FAIL:-0}" = 1 ]; then
    printf 'remote command failed\n' >&2
    exit 7
fi
# the remote command is always the last argument
cmd=""
for a in "$@"; do
    cmd="$a"
done
case "$cmd" in
    *"MIRA_KEY_INSTALL"*)
        f="${FAKE_PI_AUTHKEYS:-/nonexistent-fake-authkeys}"
        [ -d "$(dirname "$f")" ] || exit 255
        touch "$f" 2>/dev/null || exit 255
        line=$(printf '%s\n' "$cmd" | sed -n 's/^LINE="\([^"]*\)"[[:space:]]*$/\1/p')
        if [ -n "$line" ] && grep -qxF "$line" "$f"; then
            echo "mira-key: already present"
        else
            printf '%s\n' "$line" >> "$f"
            echo "mira-key: installed"
        fi
        exit 0
        ;;
    *)
        exit 0
        ;;
esac
`

const fakeSSHPass = `#!/bin/sh
# fake sshpass: drops its own flags and the ssh word, then execs the fake
# ssh (same PATH dir) in password mode (no -i)
[ "${1:-}" = "-e" ] && shift
[ "${1:-}" = "ssh" ] && shift
exec ssh "$@"
`

const fakeSSHKeygen = `#!/bin/sh
# fake ssh-keygen:
#   -y -f <path>                          -> prints the fake public key
#   -q -t ed25519 -N "" -C c -f <path>   -> writes <path> + <path>.pub
f=""
y=0
while [ $# -gt 0 ]; do
    case "$1" in
        -f) f="${2:-}"; shift 2 ;;
        -y) y=1; shift ;;
        *) shift ;;
    esac
done
if [ "$y" = 1 ]; then
    printf 'ssh-ed25519 AAAAFakeKey fake@fake\n'
    exit 0
fi
[ -n "$f" ] || exit 1
[ ! -e "$f" ] || { printf 'already exists\n' >&2; exit 1; }
printf 'FAKE-PRIVATE-KEY\n' > "$f" || exit 1
printf 'ssh-ed25519 AAAAFakeKey fake@fake\n' > "$f.pub" || exit 1
exit 0
`

// fakeSSHBinDir installs the fake ssh/sshpass/ssh-keygen executables and
// points PATH at them. t.Setenv => the caller must not use t.Parallel.
func fakeSSHBinDir(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	for name, body := range map[string]string{
		"ssh":        fakeSSH,
		"sshpass":    fakeSSHPass,
		"ssh-keygen": fakeSSHKeygen,
	} {
		p := filepath.Join(bin, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatalf("writing fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fakeKeyEnv sets the key path to a temp location and the fake
// authorized_keys file; returns both paths.
func fakeKeyEnv(t *testing.T) (keyPath, authKeys string) {
	t.Helper()
	keyPath = filepath.Join(t.TempDir(), "id_ed25519")
	t.Setenv(sshKeyPathEnv, keyPath)
	authKeys = filepath.Join(t.TempDir(), "authorized_keys")
	t.Setenv("FAKE_PI_AUTHKEYS", authKeys)
	t.Setenv("FAKE_SSH_KEY_OK", "0")
	return keyPath, authKeys
}

func fakeKeyOK(t *testing.T, ok bool) {
	t.Helper()
	if ok {
		t.Setenv("FAKE_SSH_KEY_OK", "1")
	} else {
		t.Setenv("FAKE_SSH_KEY_OK", "0")
	}
}

const fakePublicKey = "ssh-ed25519 AAAAFakeKey fake@fake"

func TestEnsureKey_GeneratesPair(t *testing.T) {
	fakeSSHBinDir(t)
	keyPath, _ := fakeKeyEnv(t)

	if err := EnsureKey(context.Background()); err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	priv, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("private key missing: %v", err)
	}
	if string(priv) != "FAKE-PRIVATE-KEY\n" {
		t.Errorf("private key content = %q, want the fake key", priv)
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatalf("public key missing: %v", err)
	}
	if strings.TrimSpace(string(pub)) != fakePublicKey {
		t.Errorf("public key = %q, want %q", pub, fakePublicKey)
	}
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("private key mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestEnsureKey_SkipsCompletePair(t *testing.T) {
	fakeSSHBinDir(t)
	keyPath, _ := fakeKeyEnv(t)
	if err := os.WriteFile(keyPath, []byte("KEEP-PRIVATE\n"), 0o600); err != nil {
		t.Fatalf("writing private key: %v", err)
	}
	if err := os.WriteFile(keyPath+".pub", []byte("KEEP-PUB\n"), 0o644); err != nil {
		t.Fatalf("writing public key: %v", err)
	}

	if err := EnsureKey(context.Background()); err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	priv, _ := os.ReadFile(keyPath)
	pub, _ := os.ReadFile(keyPath + ".pub")
	// the fake ssh-keygen must not have rewritten a working pair
	if string(priv) != "KEEP-PRIVATE\n" {
		t.Errorf("private key overwritten: %q", priv)
	}
	if strings.TrimSpace(string(pub)) != "KEEP-PUB" {
		t.Errorf("public key overwritten: %q", pub)
	}
}

func TestEnsureKey_DerivesMissingPub(t *testing.T) {
	fakeSSHBinDir(t)
	keyPath, _ := fakeKeyEnv(t)
	if err := os.WriteFile(keyPath, []byte("PRIVATE-ONLY\n"), 0o600); err != nil {
		t.Fatalf("writing private key: %v", err)
	}

	if err := EnsureKey(context.Background()); err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	priv, _ := os.ReadFile(keyPath)
	if string(priv) != "PRIVATE-ONLY\n" {
		t.Errorf("private key changed: %q", priv)
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatalf("public key missing after derive: %v", err)
	}
	if strings.TrimSpace(string(pub)) != fakePublicKey {
		t.Errorf("derived public key = %q, want %q", pub, fakePublicKey)
	}
}

func TestEnsureKey_RegeneratesMissingPrivate(t *testing.T) {
	fakeSSHBinDir(t)
	keyPath, _ := fakeKeyEnv(t)
	// stale .pub without its private key
	if err := os.WriteFile(keyPath+".pub", []byte("STALE-PUB\n"), 0o644); err != nil {
		t.Fatalf("writing public key: %v", err)
	}

	if err := EnsureKey(context.Background()); err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	priv, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("private key missing after regeneration: %v", err)
	}
	if string(priv) != "FAKE-PRIVATE-KEY\n" {
		t.Errorf("private key = %q, want the fresh fake key", priv)
	}
	pub, _ := os.ReadFile(keyPath + ".pub")
	if strings.TrimSpace(string(pub)) != fakePublicKey {
		t.Errorf("stale public key survived: %q", pub)
	}
}

func TestRunKeyFirst_KeySuccess(t *testing.T) {
	fakeSSHBinDir(t)
	_, _ = fakeKeyEnv(t)
	fakeKeyOK(t, true)

	usedKey, _, err := RunKeyFirst(context.Background(), "192.168.7.1", "u", "pw", "true")
	if err != nil {
		t.Fatalf("RunKeyFirst: %v", err)
	}
	if !usedKey {
		t.Errorf("usedKey = false, want true (key auth served the run)")
	}
}

func TestRunKeyFirst_KeyFailsPasswordFallback(t *testing.T) {
	fakeSSHBinDir(t)
	_, _ = fakeKeyEnv(t)
	fakeKeyOK(t, false) // key auth fails -> password fallback must succeed

	usedKey, _, err := RunKeyFirst(context.Background(), "192.168.7.1", "u", "pw", "true")
	if err != nil {
		t.Fatalf("RunKeyFirst: %v", err)
	}
	if usedKey {
		t.Errorf("usedKey = true, want false (password fallback served the run)")
	}
}

func TestRunKeyFirst_BothFail(t *testing.T) {
	fakeSSHBinDir(t)
	_, _ = fakeKeyEnv(t)
	fakeKeyOK(t, false)
	t.Setenv("FAKE_SSH_PASS_FAIL", "1")

	usedKey, _, err := RunKeyFirst(context.Background(), "192.168.7.1", "u", "s3cret-pw", "true")
	if err == nil {
		t.Fatal("RunKeyFirst succeeded, want a failure")
	}
	if usedKey {
		t.Errorf("usedKey = true, want false")
	}
	// both attempts are reported, and the password must not leak
	msg := err.Error()
	if !strings.Contains(msg, "key:") || !strings.Contains(msg, "password:") {
		t.Errorf("error = %q, want both attempts reported", msg)
	}
	if strings.Contains(msg, "s3cret-pw") {
		t.Errorf("password leaked into error: %q", msg)
	}
}

func TestRunKeyFirst_RemoteCommandFailureNoFallback(t *testing.T) {
	fakeSSHBinDir(t)
	_, _ = fakeKeyEnv(t)
	fakeKeyOK(t, true)
	t.Setenv("FAKE_SSH_CMD_FAIL", "1") // key auth ok, remote command exits 7

	// the command already ran - a password retry would double-execute it,
	// so RunKeyFirst must return the failure. (If it fell back, the fake
	// password mode would succeed and err would be nil.)
	usedKey, _, err := RunKeyFirst(context.Background(), "192.168.7.1", "u", "pw", "true")
	if err == nil {
		t.Fatal("RunKeyFirst succeeded, want the remote command failure")
	}
	if usedKey {
		t.Errorf("usedKey = true, want false")
	}
}

func TestInstallKeyCommand(t *testing.T) {
	cmd, err := installKeyCommand(fakePublicKey)
	if err != nil {
		t.Fatalf("installKeyCommand: %v", err)
	}
	if !strings.Contains(cmd, "MIRA_KEY_INSTALL") {
		t.Errorf("missing the MIRA_KEY_INSTALL marker: %q", cmd)
	}
	if !strings.Contains(cmd, `LINE="`+fakePublicKey+`"`) {
		t.Errorf("public key not embedded: %q", cmd)
	}
	if strings.Contains(cmd, "__MIRA_PUBKEY__") {
		t.Errorf("placeholder survived: %q", cmd)
	}
	for _, bad := range []string{`a"b`, "a$b", "a`b", "a\nb", "", "   "} {
		if _, err := installKeyCommand(bad); err == nil {
			t.Errorf("unsafe public key %q accepted", bad)
		}
	}
}
