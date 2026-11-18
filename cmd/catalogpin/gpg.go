package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// errGPGLocal marks a gpg failure that says nothing about the signature: no
// gpg, no room for its home, a committed key that will not import.
var errGPGLocal = errors.New("local gpg failure")

const (
	// statusPrefix starts every line gpg writes to --status-fd.
	statusPrefix = "[GNUPG:] "
	// validSigFields is the field count of a VALIDSIG status line without its
	// prefix: the keyword, then fingerprint, date, timestamp, expiry, version,
	// reserved, public-key algorithm, hash algorithm, class and the primary
	// key's fingerprint.
	validSigFields = 11
	// fingerprintLen is the length of a v4 fingerprint in hex digits.
	fingerprintLen = 40
	// primaryField is the index of the fingerprint in a "fpr" colon record.
	primaryField = 9
)

// isFingerprint reports whether s is 40 upper-case hex digits, as gpg prints a
// v4 fingerprint.
func isFingerprint(s string) bool {
	if len(s) != fingerprintLen {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// parseValidSig returns the primary-key fingerprint of the first VALIDSIG
// line of gpg's status output, which is its last field. It returns false when
// there is none, when EXPKEYSIG, REVKEYSIG or BADSIG appears anywhere (gpg exits
// 0 for a signature by an expired key), and when PLAINTEXT or PLAINTEXT_LENGTH
// does: a detached signature carries no data of its own, so the status of one
// that did is not a verdict on the data file.
func parseValidSig(status []byte) (string, bool) {
	primary := ""
	for line := range strings.Lines(string(status)) {
		rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), statusPrefix)
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "EXPKEYSIG", "REVKEYSIG", "BADSIG", "PLAINTEXT", "PLAINTEXT_LENGTH":
			return "", false
		case "VALIDSIG":
			if last := fields[len(fields)-1]; primary == "" && len(fields) >= validSigFields && isFingerprint(last) {
				primary = last
			}
		}
	}
	return primary, primary != ""
}

// keyFingerprint returns the fingerprint of the first key in gpg's
// --with-colons listing: the "fpr" record that follows the first "pub"
// record. A subkey record before it is an error, because its fpr would be the
// subkey's.
func keyFingerprint(colons []byte) (string, error) {
	seenPub := false
	for line := range strings.Lines(string(colons)) {
		fields := strings.Split(strings.TrimRight(line, "\r\n"), ":")
		switch fields[0] {
		case "pub":
			if seenPub {
				return "", errors.New("a pub record has no fpr record after it")
			}
			seenPub = true
		case "sub", "ssb", "sec":
			if seenPub {
				return "", fmt.Errorf("a %s record comes before the pub record's fpr", fields[0])
			}
		case "fpr":
			if !seenPub {
				continue
			}
			if len(fields) <= primaryField || !isFingerprint(fields[primaryField]) {
				return "", errors.New("the fpr record after pub holds no 40-digit fingerprint")
			}
			return fields[primaryField], nil
		}
	}
	return "", errors.New("no pub record with an fpr record after it")
}

// runGPG runs gpg with the given home and --batch, and returns its standard
// output. A failure carries gpg's standard error on one line.
func runGPG(ctx context.Context, home string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gpg", append([]string{"--homedir", home, "--batch"}, args...)...) //nolint:gosec // G204: the program is fixed; the arguments are a temporary home and the paths this tool made or was given
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gpg: %w: %s", err, strings.Join(strings.Fields(stderr.String()), " "))
	}
	return stdout.Bytes(), nil
}

// gpgVerify checks the detached signature in sigFile over dataFile with the
// public key in keyFile, in a fresh gpg home made next to dataFile, and
// returns the primary fingerprint of the key that signed. gpg must exit 0 and
// report a VALIDSIG with no expired, revoked or bad signature.
func gpgVerify(ctx context.Context, keyFile, sigFile, dataFile string) (fpr string, err error) {
	home, err := os.MkdirTemp(filepath.Dir(dataFile), "gnupg-")
	if err != nil {
		return "", fmt.Errorf("%w: make a gpg home: %w", errGPGLocal, err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(home)) }()
	if _, err := runGPG(ctx, home, "--import", keyFile); err != nil {
		return "", fmt.Errorf("%w: import %s: %w", errGPGLocal, filepath.Base(keyFile), err)
	}
	status, err := runGPG(ctx, home, "--status-fd", "1", "--verify", sigFile, dataFile)
	if _, exited := errors.AsType[*exec.ExitError](err); err != nil && !exited { //nolint:errcheck // the match target is not a dropped error; modernize requires AsType over errors.As
		return "", fmt.Errorf("%w: verify %s: %w", errGPGLocal, filepath.Base(dataFile), err)
	}
	if err != nil {
		return "", fmt.Errorf("verify %s: %w", filepath.Base(dataFile), err)
	}
	fpr, ok := parseValidSig(status)
	if !ok {
		return "", errors.New("gpg reported no valid signature (none, or an expired, revoked or bad one)")
	}
	return fpr, nil
}

// gpgKeyPrimary returns the primary fingerprint of the first key in keyFile,
// using a fresh gpg home made in workDir. Every failure is local (errGPGLocal):
// the key file is the repository's own, so a failure to read it is not a verdict
// on any signature.
func gpgKeyPrimary(ctx context.Context, workDir, keyFile string) (fpr string, err error) {
	home, err := os.MkdirTemp(workDir, "gnupg-")
	if err != nil {
		return "", fmt.Errorf("%w: make a gpg home: %w", errGPGLocal, err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(home)) }()
	colons, err := runGPG(ctx, home, "--with-colons", "--show-keys", keyFile)
	if err != nil {
		return "", fmt.Errorf("%w: list %s: %w", errGPGLocal, filepath.Base(keyFile), err)
	}
	fpr, err = keyFingerprint(colons)
	if err != nil {
		return "", fmt.Errorf("%w: read %s: %w", errGPGLocal, filepath.Base(keyFile), err)
	}
	return fpr, nil
}
