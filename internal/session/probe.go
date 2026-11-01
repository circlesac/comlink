package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// sshConfigured reports whether ssh applies any of the configuration in the
// ssh config file at sshConfig (or a file it Includes) to the host name: a
// Host block or pattern, or a Match block. It compares `ssh -G` for name with
// `ssh -G` for a random name nothing matches, each with the probed name
// replaced in its own output. -F names the file because ssh reads
// ~/.ssh/config from the passwd home, not $HOME, and it is the file the
// Include line goes into. When ssh is not on PATH or either probe fails the
// answer is false: the Codex app needs ssh anyway. A short name that only
// resolves through DNS search domains has no configuration and is not
// detected.
func sshConfigured(sshConfig, name string) bool {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		return false
	}
	got, err := sshSettings(ssh, sshConfig, name)
	if err != nil {
		return false
	}
	random := make([]byte, 8)
	_, _ = rand.Read(random)
	unmatched, err := sshSettings(ssh, sshConfig, "comlink-probe-"+hex.EncodeToString(random))
	if err != nil {
		return false
	}
	return got != unmatched
}

// sshSettings is the output of `ssh -G -F sshConfig name` with every word
// equal to name (in any case: ssh lowercases the hostname) replaced by a
// placeholder, so the outputs for two names can be compared.
func sshSettings(ssh, sshConfig, name string) (string, error) {
	out, err := exec.Command(ssh, "-G", "-F", sshConfig, "--", name).Output()
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(out), "\n")
	for i, line := range lines {
		words := strings.Fields(line)
		for j, w := range words {
			if strings.EqualFold(w, name) {
				words[j] = "%n"
			}
		}
		lines[i] = strings.Join(words, " ")
	}
	return strings.Join(lines, "\n"), nil
}

// checkSSHConfig asks ssh whether the user's configuration in sshConfig
// already applies to name, which the session's Host block would shadow
// while it runs. For a derived name that is an error; for one the user
// chose (derived false) it is a warning on stderr. A name whose comlink ssh
// config file is still there (up, or left by a session that did not stop)
// is not checked: that file is comlink's own and would make the probes
// differ, and Start deals with it.
func (s *Store) checkSSHConfig(sshConfig, name string, derived bool, stderr io.Writer) error {
	if _, err := os.Lstat(s.confPath(name)); err == nil || !sshConfigured(sshConfig, name) {
		return nil
	}
	why := fmt.Sprintf("%q already has ssh configuration (ssh -G %s differs from an unmatched host)", name, name)
	if derived {
		return fmt.Errorf("%s; pass --name NAME to pick another alias", why)
	}
	_, _ = fmt.Fprintf(stderr, "comlink: warning: %s; this session's Host block takes precedence while it runs\n", why)
	return nil
}
