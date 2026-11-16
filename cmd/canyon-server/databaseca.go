package main

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

// databaseCAOf reads CANYON_APP_DATABASE_CA_FILE, the apps database's CA bundle: every Kubernetes
// /run carries it as database_ca and each app's DATABASE_URL verifies the server against it
// (contracts/executor.md, contracts/provisioner.md). Unset is "". Set, it fails the boot without the
// built-in driver, on Docker, on a file it cannot read, and on a bundle that is not certificates
// alone — a private key beside them is refused, never handed to every app. The certificates come back
// re-encoded, 64 characters a line and nothing between them, at most 256 KiB: /run's database_ca.
func databaseCAOf(file, appDB string, kube bool) (string, error) {
	switch {
	case file == "":
		return "", nil
	case appDB == "":
		return "", errors.New("CANYON_APP_DATABASE_CA_FILE needs CANYON_APP_DATABASE_URL: it is that server's CA")
	case !kube:
		return "", errors.New("CANYON_APP_DATABASE_CA_FILE needs CANYON_RUNTIME=kubernetes: a Docker app is handed no CA")
	}
	raw, err := os.ReadFile(file) //nolint:gosec // the host's own setting, named by the operator, never by a request
	if err != nil {
		return "", fmt.Errorf("CANYON_APP_DATABASE_CA_FILE: %w", err)
	}
	var out []byte
	for block, rest := pem.Decode(raw); block != nil; block, rest = pem.Decode(rest) {
		if block.Type != "CERTIFICATE" {
			return "", fmt.Errorf("CANYON_APP_DATABASE_CA_FILE holds a %s block: it takes certificates alone", block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return "", fmt.Errorf("CANYON_APP_DATABASE_CA_FILE: %w", err)
		}
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})...)
	}
	switch {
	case len(out) == 0:
		return "", errors.New("CANYON_APP_DATABASE_CA_FILE holds no PEM certificate")
	case len(out) > 256<<10:
		return "", errors.New("CANYON_APP_DATABASE_CA_FILE holds over 256 KiB of certificates, more than a /run carries")
	}
	return string(out), nil
}
