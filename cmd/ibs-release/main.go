// Command ibs-release prepares IBS release files. It never sees a private
// key: OpenSSL signs (see scripts/release.sh and DEVELOPMENT.md).
//
//	ibs-release manifest -version V [-platform linux/amd64] [-notes FILE] PROGRAM > manifest.json
//	openssl pkeyutl -sign -rawin -inkey KEY -in manifest.json -out manifest.sig
//	ibs-release pack [-trust ibspub1:…] -o OUT.ibsrelease PROGRAM manifest.json manifest.sig
//	openssl pkey -in KEY -pubout | ibs-release pubkey
//
// manifest describes the program: version, platform, SHA-256, size, and the
// oldest version and database level it works with. pack checks that the
// three belong together and that the signature is made with the built-in
// release key (or a key given with -trust, for builds of your own), and writes
// the .ibsrelease file sites install. pubkey prints the ibspub1 form of a
// public key, for internal/release/keys.go or a site's Release keys.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"ibs/internal/db"
	"ibs/internal/release"
	"ibs/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("usage: ibs-release manifest|pack|pubkey ..."))
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	ver := fs.String("version", "", "version of the program (manifest)")
	platform := fs.String("platform", "linux/amd64", "platform the program is built for (manifest)")
	out := fs.String("o", "", "the .ibsrelease file to write (pack)")
	notes := fs.String("notes", "", "file with the release notes, from the changelog (manifest)")
	requiresAge := fs.Bool("requires-age", false, "this version reads only age: sites install it once converted (manifest)")
	var trust []string
	fs.Func("trust", "also accept a signature by this key, ibspub1:… (pack)", func(v string) error {
		trust = append(trust, v)
		return nil
	})
	fs.Parse(os.Args[2:])

	switch os.Args[1] {
	case "manifest":
		if fs.NArg() != 1 || *ver == "" {
			fail(errors.New("usage: ibs-release manifest -version V [-platform P] PROGRAM"))
		}
		program := read(fs.Arg(0))
		sum := sha256.Sum256(program)
		m := release.Manifest{Version: *ver, Platform: *platform, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(program)),
			MinCompatible: version.MinCompatible, DBLevel: db.Level, Released: time.Now().Unix()}
		m.RequiresAge = *requiresAge
		if *notes != "" {
			m.Notes = strings.TrimSpace(string(read(*notes)))
		}
		b, _ := json.MarshalIndent(m, "", "  ")
		os.Stdout.Write(b)
	case "pack":
		if fs.NArg() != 3 || *out == "" {
			fail(errors.New("usage: ibs-release pack [-trust KEY] -o OUT.ibsrelease PROGRAM MANIFEST SIGNATURE"))
		}
		keys := []release.Key{release.DefaultKey}
		for _, t := range trust {
			k, err := release.ParseKey(t)
			if err != nil {
				fail(fmt.Errorf("-trust: %w", err))
			}
			keys = append(keys, k)
		}
		b := release.Bundle{Program: read(fs.Arg(0)), ManifestJSON: read(fs.Arg(1)), Signature: read(fs.Arg(2))}
		m, k, err := b.Check(keys)
		if err != nil {
			fail(fmt.Errorf("%w (is it signed with the release key? built in: %s)", err, release.DefaultKey))
		}
		if err := os.WriteFile(*out, b.BundleBytes(), 0o644); err != nil {
			fail(err)
		}
		fmt.Printf("packed %s %s, signed with key %s -> %s\n", m.Version, m.Platform, k.ID, *out)
	case "pubkey":
		in, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<16))
		if err != nil {
			fail(err)
		}
		block, _ := pem.Decode(in)
		if block == nil {
			fail(errors.New("expected a PEM public key on stdin: openssl pkey -in KEY -pubout | ibs-release pubkey"))
		}
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		pub, ok := k.(ed25519.PublicKey)
		if err != nil || !ok {
			fail(errors.New("not an Ed25519 public key"))
		}
		fmt.Println(release.Key{ID: release.KeyID(pub), Pub: pub})
	default:
		fail(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func read(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	return b
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "ibs-release:", err)
	os.Exit(1)
}
