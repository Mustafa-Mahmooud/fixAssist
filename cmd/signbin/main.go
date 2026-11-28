// Command signbin signs the auto-fix daemon's downloads so daemons can
// update themselves safely (see internal/daemonver).
//
//	signbin keygen PRIVATE-KEY-FILE     writes a new key; prints the public key
//	signbin sign KEY-FILE VERSION FILE…  writes FILE.sig for each file
//	signbin verify SIG-FILE…             checks .sig files against the key
//	                                     built into daemons (after a deploy:
//	                                     is the right key in Northflank?)
//
// The private key never goes into the repository or the running server:
// it's a Northflank build secret file, used only in the Dockerfile's build
// stage.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yourname/fixassist/internal/daemonver"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "signbin:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: signbin keygen KEY-FILE | signbin sign KEY-FILE VERSION FILE...")
	}
	switch args[0] {
	case "keygen":
		if len(args) != 2 {
			return errors.New("usage: signbin keygen KEY-FILE")
		}
		if _, err := os.Stat(args[1]); err == nil {
			return fmt.Errorf("%s exists: not overwriting a signing key", args[1])
		}
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(args[1]), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(args[1], []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Println(base64.StdEncoding.EncodeToString(pub))
		return nil
	case "sign":
		if len(args) < 4 {
			return errors.New("usage: signbin sign KEY-FILE VERSION FILE...")
		}
		b, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return errors.New("the key file isn't a signbin key")
		}
		key := ed25519.NewKeyFromSeed(seed)
		for _, f := range args[3:] {
			bin, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			if err := os.WriteFile(f+".sig", daemonver.Sign(key, filepath.Base(f), args[2], bin), 0o644); err != nil {
				return err
			}
		}
		return nil
	case "verify":
		if len(args) < 2 {
			return errors.New("usage: signbin verify SIG-FILE...")
		}
		for _, f := range args[1:] {
			b, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			m, err := daemonver.Open(b)
			if err != nil {
				return fmt.Errorf("%s: %w (not signed with the key daemons trust)", f, err)
			}
			fmt.Printf("%s: ok, %s %s\n", f, m.File, m.Version)
		}
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}
