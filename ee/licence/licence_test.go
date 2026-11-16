package licence

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func key(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

var meridian = Payload{Customer: "Meridian Insurance", Issued: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC), Expires: time.Date(2027, 11, 14, 0, 0, 0, 0, time.UTC),
	Seats: 50, Features: []string{"outbound_keys", "platform_patching", "group_sync"}}

// The contract's golden file verifies under the golden public key and reads as the golden payload: the server reads
// the seam's file, not a shape of its own (contracts/testdata/licence).
func TestTheGoldenLicenceVerifies(t *testing.T) {
	raw, err := os.ReadFile("../../contracts/testdata/licence/licence.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		File      json.RawMessage `json:"file"`
		PublicKey string          `json:"public_key"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	pub, err := base64.StdEncoding.DecodeString(golden.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	p, problem := Verify(golden.File, []ed25519.PublicKey{pub})
	if problem != "" || p.Customer != meridian.Customer || !p.Expires.Equal(meridian.Expires) || p.Seats != 50 || len(p.Features) != 3 {
		t.Fatalf("%+v %q", p, problem)
	}
}

// A licence signed by any key of the list verifies, so a key can be rotated; one signed by none is unsigned, and a
// changed payload or signature is unsigned too.
func TestALicenceVerifiesUnderAnyListedKeyAndNoOther(t *testing.T) {
	old, oldPriv := key(t)
	cur, _ := key(t)
	_, otherPriv := key(t)
	text, err := Sign(oldPriv, meridian)
	if err != nil {
		t.Fatal(err)
	}
	if _, problem := Verify(text, []ed25519.PublicKey{cur, old}); problem != "" {
		t.Errorf("signed by a listed key: %q", problem)
	}
	foreign, _ := Sign(otherPriv, meridian)
	if _, problem := Verify(foreign, []ed25519.PublicKey{cur, old}); problem != Unsigned {
		t.Errorf("signed by a key not listed: %q", problem)
	}
	if _, problem := Verify(text, nil); problem != Unsigned {
		t.Errorf("no key at all: %q", problem)
	}
	var f file
	_ = json.Unmarshal(text, &f)
	raw, _ := base64.StdEncoding.DecodeString(f.Payload)
	raw[len(raw)-3] = '9' // the payload's last digit of seats, or of a date: any byte changed is a payload Canyon didn't sign
	tampered, _ := json.Marshal(file{base64.StdEncoding.EncodeToString(raw), f.Signature})
	if _, problem := Verify(tampered, []ed25519.PublicKey{old}); problem != Unsigned {
		t.Errorf("a changed payload: %q", problem)
	}
}

// Anything that is not the contract's file, or a signed payload that is not its payload, is unreadable.
func TestWhatIsNoLicenceFileIsUnreadable(t *testing.T) {
	pub, priv := key(t)
	sign := func(raw string) []byte {
		out, _ := json.Marshal(file{base64.StdEncoding.EncodeToString([]byte(raw)), base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(raw)))})
		return out
	}
	good, _ := Sign(priv, meridian)
	for name, text := range map[string][]byte{
		"empty":                          nil,
		"not JSON":                       []byte("licence"),
		"a third field":                  append(good[:len(good)-1:len(good)-1], []byte(`,"key_id":"2026"}`)...),
		"trailing data":                  append(append([]byte{}, good...), []byte(`{}`)...),
		"a short signature":              []byte(`{"payload":"e30=","signature":"AAAA"}`),
		"a payload not in base64":        []byte(`{"payload":"{}","signature":"` + base64.StdEncoding.EncodeToString(make([]byte, 64)) + `"}`),
		"a field Canyon didn't sign for": sign(`{"customer":"Meridian Insurance","issued":"2026-11-14T00:00:00Z","expires":"2027-11-14T00:00:00Z","seats":50,"features":["group_sync"],"hosts":["x"]}`),
		"an expiry before it was issued": sign(`{"customer":"Meridian Insurance","issued":"2027-11-14T00:00:00Z","expires":"2026-11-14T00:00:00Z","seats":50,"features":["group_sync"]}`),
		"no feature":                     sign(`{"customer":"Meridian Insurance","issued":"2026-11-14T00:00:00Z","expires":"2027-11-14T00:00:00Z","seats":50,"features":[]}`),
		"a feature twice":                sign(`{"customer":"Meridian Insurance","issued":"2026-11-14T00:00:00Z","expires":"2027-11-14T00:00:00Z","seats":50,"features":["group_sync","group_sync"]}`),
		"a date with an offset":          sign(`{"customer":"Meridian Insurance","issued":"2026-11-14T00:00:00+01:00","expires":"2027-11-14T00:00:00Z","seats":50,"features":["group_sync"]}`),
		"no seats":                       sign(`{"customer":"Meridian Insurance","issued":"2026-11-14T00:00:00Z","expires":"2027-11-14T00:00:00Z","seats":0,"features":["group_sync"]}`),
	} {
		if _, problem := Verify(text, []ed25519.PublicKey{pub}); problem != Unreadable {
			t.Errorf("%s: %q, want unreadable", name, problem)
		}
	}
}

// valid until it expires, grace for 30 days after, lapsed from then on.
func TestALicenceIsValidThenInGraceThenLapsed(t *testing.T) {
	for _, c := range []struct {
		at   time.Time
		want string
	}{
		{meridian.Expires.Add(-time.Second), "valid"},
		{meridian.Expires, "grace"},
		{meridian.Expires.Add(Grace - time.Second), "grace"},
		{meridian.Expires.Add(Grace), "lapsed"},
	} {
		if got := meridian.State(c.at); got != c.want {
			t.Errorf("at %s: %s, want %s", c.at, got, c.want)
		}
	}
}
