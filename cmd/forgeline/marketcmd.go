package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/forgelinehq/forgeline/internal/server"
)

const marketHelp = `Usage:
  forgeline market keygen NAME
        make a signing key: writes NAME.key (mode 0600) and prints the line to put in
        FORGELINE_MARKET_KEYS on the servers that should trust it
  forgeline market sign -key NAME.key [-id NAME] manifest.json
        fill in the manifest's sha256 and write manifest.sig beside it
`

// runMarket is the publisher's side of signed packages (design note 019).
func runMarket(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, marketHelp)
		return 2
	}
	switch args[0] {
	case "keygen":
		return marketKeygen(args[1:], stdout, stderr)
	case "sign":
		return marketSign(args[1:], stdout, stderr)
	}
	fmt.Fprint(stderr, marketHelp)
	return 2
}

func marketKeygen(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || !validKeyName(args[0]) {
		fmt.Fprint(stderr, marketHelp)
		return 2
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	path := args[0] + ".key"
	// O_EXCL: never overwrite a key.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	_, werr := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv.Seed()))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", werr)
		return 1
	}
	fmt.Fprintf(stdout, "Wrote %s. Keep it secret.\nTrust it on a server with:\n  FORGELINE_MARKET_KEYS=%s=%s\n", path, args[0], base64.StdEncoding.EncodeToString(pub))
	return 0
}

func validKeyName(s string) bool {
	// The same rule as the server's: a lowercase letter or digit first, then letters, digits . _ -
	if s == "" || len(s) > 39 || !(s[0] >= 'a' && s[0] <= 'z' || s[0] >= '0' && s[0] <= '9') {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_') {
			return false
		}
	}
	return true
}

func marketSign(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("market sign", flag.ContinueOnError)
	fs.SetOutput(stderr)
	keyFile := fs.String("key", "", "the NAME.key file from keygen")
	id := fs.String("id", "", "the key id to record (default: the key file's name)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *keyFile == "" {
		fmt.Fprint(stderr, marketHelp)
		return 2
	}
	name := *id
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(*keyFile), ".key")
	}
	raw, err := os.ReadFile(*keyFile)
	if err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		fmt.Fprintln(stderr, "forgeline: the key file is not a forgeline market key")
		return 1
	}
	manifest, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	signed, sig, err := server.SignManifest(manifest, name, ed25519.NewKeyFromSeed(seed))
	if err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	sigPath := strings.TrimSuffix(fs.Arg(0), ".json") + ".sig"
	if err := os.WriteFile(fs.Arg(0), signed, 0o644); err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	if err := os.WriteFile(sigPath, sig, 0o644); err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Signed %s as %q; wrote %s.\n", fs.Arg(0), name, sigPath)
	return 0
}
