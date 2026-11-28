// Package daemonver is what the auto-fix daemon (cmd/fixassist) and the
// server agree on about daemon updates: the version this commit builds, the
// key that signs its binaries, and how a signature is checked.
//
// The daemon replaces itself only with a binary whose signed manifest names
// a version strictly newer than its own. The private key lives only in the
// build (a Northflank build secret file), never in the running server, so
// someone who breaks into the server can't hand founders a binary of their
// own, nor an older signed one.
package daemonver

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version is the daemon this commit builds.
const Version = "0.4.0"

// publicKey verifies update manifests (base64 ed25519). It's a var only so a
// test build can swap it with -ldflags "-X ...daemonver.publicKey=..."; no
// flag, setting or environment variable can change it at run time.
var publicKey = "WreMrwDQ1wbjtgt6es4ollVEnQxXyXfdXKbIxgD7yJg="

// Manifest is what is signed for one binary: "<file> <version> <sha256>".
type Manifest struct {
	File, Version, SHA256 string
}

func (m Manifest) String() string { return m.File + " " + m.Version + " " + m.SHA256 }

// Sign makes a .sig file's content: the manifest line, then its signature.
func Sign(key ed25519.PrivateKey, file, version string, binary []byte) []byte {
	sum := sha256.Sum256(binary)
	m := Manifest{File: file, Version: version, SHA256: hex.EncodeToString(sum[:])}.String()
	return []byte(m + "\n" + base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(m))) + "\n")
}

// ErrBadSignature is a manifest the built-in key didn't sign.
var ErrBadSignature = errors.New("the update's signature doesn't match")

// Open checks a .sig file against the built-in key and returns its manifest.
func Open(sig []byte) (Manifest, error) {
	return open(sig, publicKey)
}

func open(sig []byte, pub string) (Manifest, error) {
	key, err := base64.StdEncoding.DecodeString(pub)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return Manifest{}, errors.New("this build has no update key")
	}
	line, s, ok := strings.Cut(strings.TrimSpace(string(sig)), "\n")
	if !ok {
		return Manifest{}, ErrBadSignature
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || !ed25519.Verify(key, []byte(line), raw) {
		return Manifest{}, ErrBadSignature
	}
	f := strings.Fields(line)
	if len(f) != 3 {
		return Manifest{}, ErrBadSignature
	}
	return Manifest{File: f[0], Version: f[1], SHA256: f[2]}, nil
}

// Matches says whether binary is the one the manifest names.
func (m Manifest) Matches(binary []byte) bool {
	sum := sha256.Sum256(binary)
	return hex.EncodeToString(sum[:]) == m.SHA256
}

// HasKey says whether this build can check updates at all.
func HasKey() bool {
	key, err := base64.StdEncoding.DecodeString(publicKey)
	return err == nil && len(key) == ed25519.PublicKeySize
}

// Older compares dotted versions ("0.3.0" < "0.4.0"); false when either
// isn't a three-part version.
func Older(v, than string) bool {
	a, b := parse(v), parse(than)
	if a == nil || b == nil {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func parse(v string) []int {
	p := strings.Split(strings.TrimSpace(v), ".")
	if len(p) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, s := range p {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return nil
		}
		out[i] = n
	}
	return out
}

// FileName is the download for a platform, as the Dockerfile names it.
func FileName(goos, goarch string) string {
	name := fmt.Sprintf("fixassist-%s-%s", goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// OpenWith checks a .sig against the given key. Only tests use it: the
// daemon checks updates with Open and the built-in key.
func OpenWith(sig []byte, pub string) (Manifest, error) {
	return open(sig, pub)
}
