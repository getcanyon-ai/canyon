package subjecthash

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Of is OpenSSL's own -subject_hash (OpenSSL 3.6.3, testdata/hashes.txt) for a CA of each subject shape: UTF8String
// with white space and capitals to fold, PrintableString, non-ASCII UTF-8, a multi-valued RDN, an IA5String
// emailAddress, a T61String read as Latin-1, a BMPString and tabs.
func TestOfIsOpenSSLsSubjectHash(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "hashes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 8 {
		t.Fatalf("%d fixtures, want the eight shapes", len(lines))
	}
	for _, line := range lines {
		name, want, _ := strings.Cut(line, " ")
		pemBytes, err := os.ReadFile(filepath.Join("testdata", name+".pem")) //nolint:gosec // the fixtures' own names
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(pemBytes)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := Of(cert.RawSubject); err != nil || got != want {
			t.Errorf("%s: %q %v, want OpenSSL's %s", name, got, err, want)
		}
	}
}

// A name that is not one DER Name hashes to nothing.
func TestOfRefusesWhatIsNoName(t *testing.T) {
	for _, raw := range [][]byte{nil, {0x30}, {0x30, 0x00, 0x00}, {0x31, 0x00}} {
		if got, err := Of(raw); err == nil {
			t.Errorf("Of(%x) = %q, want an error", raw, got)
		}
	}
}
