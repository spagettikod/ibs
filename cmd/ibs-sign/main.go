// Command ibs-sign signs IBS releases with an Ed25519 key.
//
//	ibs-sign pubkey [-key FILE]          print the public key (ibspub1:…) of a key file
//	ibs-sign sign [-key FILE] -version V [-platform linux/amd64] -o OUT.ibsrelease PROGRAM
//
// Create the key with OpenSSL (see DEVELOPMENT.md):
//
//	openssl genpkey -algorithm ed25519 -out ~/.config/ibs/signing.key
//
// The key file defaults to $IBS_SIGNING_KEY or ~/.config/ibs/signing.key. Keep
// it offline and backed up: everyone who trusts it runs what it signs. Key
// files made by earlier versions of ibs-sign (ibspriv1:…) still work.
package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ibs/internal/db"
	"ibs/internal/release"
	"ibs/internal/version"
)

const privPrefix = "ibspriv1:" // key files made by earlier versions

func defaultKeyFile() string {
	if f := os.Getenv("IBS_SIGNING_KEY"); f != "" {
		return f
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ibs", "signing.key")
}

// readKey reads an Ed25519 private key: PEM from OpenSSL, or the ibspriv1
// format of earlier versions.
func readKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no signing key at %s; create one with: openssl genpkey -algorithm ed25519 -out %s", path, path)
	}
	if err != nil {
		return nil, err
	}
	if block, _ := pem.Decode(b); block != nil {
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if priv, ok := k.(ed25519.PrivateKey); err == nil && ok {
			return priv, nil
		}
		return nil, fmt.Errorf("%s is not an Ed25519 key (create one with: openssl genpkey -algorithm ed25519)", path)
	}
	seed, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(string(b)), privPrefix))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not an Ed25519 key", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func publicOf(priv ed25519.PrivateKey) release.Key {
	pub := priv.Public().(ed25519.PublicKey)
	return release.Key{ID: release.KeyID(pub), Pub: pub}
}

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("usage: ibs-sign pubkey|sign ..."))
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	keyFile := fs.String("key", defaultKeyFile(), "signing key file")
	ver := fs.String("version", "", "version of the program (sign)")
	platform := fs.String("platform", "linux/amd64", "platform the program is built for (sign)")
	out := fs.String("o", "", "output .ibsrelease file (sign)")
	fs.Parse(os.Args[2:])

	switch os.Args[1] {
	case "pubkey":
		priv, err := readKey(*keyFile)
		if err != nil {
			fail(err)
		}
		fmt.Println(publicOf(priv))
	case "sign":
		if fs.NArg() != 1 || *ver == "" || *out == "" {
			fail(errors.New("usage: ibs-sign sign -version V -o OUT.ibsrelease PROGRAM"))
		}
		priv, err := readKey(*keyFile)
		if err != nil {
			fail(err)
		}
		program, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			fail(err)
		}
		b := release.MakeBundle(priv, program, release.Manifest{
			Version: *ver, Platform: *platform, MinCompatible: version.MinCompatible,
			DBLevel: db.Level, Released: time.Now().Unix(),
		})
		if err := os.WriteFile(*out, b.BundleBytes(), 0o644); err != nil {
			fail(err)
		}
		fmt.Printf("signed %s %s with key %s -> %s\n", *ver, *platform, publicOf(priv).ID, *out)
	default:
		fail(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "ibs-sign:", err)
	os.Exit(1)
}
