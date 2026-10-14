// Command ibs-sign creates release signing keys and signs IBS releases.
//
//	ibs-sign keygen [-key FILE]          create a key; prints its public half
//	ibs-sign pubkey [-key FILE]          print the public key of a key file
//	ibs-sign sign [-key FILE] -version V [-platform linux/amd64] -o OUT.ibsrelease PROGRAM
//
// The key file defaults to $IBS_SIGNING_KEY or ~/.config/ibs/signing.key. Keep
// it offline and backed up: everyone who trusts it runs what it signs.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
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

const privPrefix = "ibspriv1:"

func defaultKeyFile() string {
	if f := os.Getenv("IBS_SIGNING_KEY"); f != "" {
		return f
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ibs", "signing.key")
}

func readKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(string(b)), privPrefix))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("not an ibs-sign key file")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func publicOf(priv ed25519.PrivateKey) release.Key {
	pub := priv.Public().(ed25519.PublicKey)
	return release.Key{ID: release.KeyID(pub), Pub: pub}
}

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("usage: ibs-sign keygen|pubkey|sign ..."))
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	keyFile := fs.String("key", defaultKeyFile(), "signing key file")
	ver := fs.String("version", "", "version of the program (sign)")
	platform := fs.String("platform", "linux/amd64", "platform the program is built for (sign)")
	out := fs.String("o", "", "output .ibsrelease file (sign)")
	fs.Parse(os.Args[2:])

	switch os.Args[1] {
	case "keygen":
		if _, err := os.Stat(*keyFile); err == nil {
			fail(fmt.Errorf("%s already exists; refusing to overwrite a signing key", *keyFile))
		}
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			fail(err)
		}
		if err := os.MkdirAll(filepath.Dir(*keyFile), 0o700); err != nil {
			fail(err)
		}
		data := privPrefix + base64.RawURLEncoding.EncodeToString(priv.Seed()) + "\n"
		if err := os.WriteFile(*keyFile, []byte(data), 0o600); err != nil {
			fail(err)
		}
		k := publicOf(priv)
		fmt.Printf("Key written to %s (keep it offline and back it up).\nPublic key (id %s):\n%s\n", *keyFile, k.ID, k)
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
