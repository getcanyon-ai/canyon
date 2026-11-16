// Command licence issues a Canyon Enterprise licence (contracts/routes.md, Licence) and prints a key's public half for
// the server's list (licenceKeys, cmd/canyon-server/licence.go). The key is Ed25519, read as one of:
//
//   - -seed, a file holding the standard base64 of the 32-byte seed, or - for stdin. Canyon's own key is the licence
//     signing key, kept in Canyon's vault: pipe it in from there, never through a file, so the seed never touches disk.
//
//   - -key, a PKCS #8 PEM file, as openssl genpkey -algorithm ed25519 writes one.
//
// -public prints the key's public half and issues nothing. The tool prints the licence alone, never the key.
package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/getcanyon-ai/canyon/ee/licence"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "licence:", err)
		os.Exit(2)
	}
}

func run(args []string, in io.Reader, out io.Writer, now time.Time) error {
	fs := flag.NewFlagSet("licence", flag.ContinueOnError)
	keyFile := fs.String("key", "", "the Ed25519 private key, a PKCS #8 PEM file (openssl genpkey -algorithm ed25519)")
	seedFile := fs.String("seed", "", "the Ed25519 key's seed, standard base64 of 32 bytes, in a file, or - for stdin")
	public := fs.Bool("public", false, "print the key's public key, base64, for licenceKeys, and issue nothing")
	customer := fs.String("customer", "", "who the licence is for")
	expires := fs.String("expires", "", "the last day it holds, YYYY-MM-DD: it expires at 00:00 UTC the day after")
	seats := fs.Int("seats", 0, "how many seats")
	features := fs.String("features", "", "the features it unlocks, comma-separated: outbound_keys, platform_patching, group_sync")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var key ed25519.PrivateKey
	var err error
	switch {
	case *keyFile != "" && *seedFile != "":
		return errors.New("-key or -seed, not both")
	case *seedFile != "":
		key, err = readSeed(*seedFile, in)
	default:
		key, err = readKey(*keyFile)
	}
	if err != nil {
		return err
	}
	if *public {
		_, err := fmt.Fprintln(out, base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
		return err
	}
	last, err := time.Parse(time.DateOnly, *expires)
	if err != nil {
		return fmt.Errorf("-expires is a date, YYYY-MM-DD: %w", err)
	}
	text, err := licence.Sign(key, licence.Payload{Customer: *customer, Issued: now.UTC().Truncate(time.Second), Expires: last.AddDate(0, 0, 1),
		Seats: *seats, Features: strings.Split(*features, ",")})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(text))
	return err
}

// readSeed reads an Ed25519 seed, standard base64 of 32 bytes, from path, or from in when path is -.
func readSeed(path string, in io.Reader) (ed25519.PrivateKey, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(io.LimitReader(in, 1<<10))
	} else {
		raw, err = os.ReadFile(path) //nolint:gosec // the operator names the seed file
	}
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("the seed is the standard base64 of 32 bytes") // never the input: it is the key
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// readKey reads a PKCS #8 PEM Ed25519 private key from path.
func readKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("-key names the private key file")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the operator names the key file
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s is not a PEM private key", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ed, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an Ed25519 key", path)
	}
	return ed, nil
}
