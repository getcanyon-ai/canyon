package cli

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"

	"github.com/jackc/pgx/v5/pgproto3"
)

// scramIterations is PBKDF2's count for the run's salted token: Postgres's own default.
const scramIterations = 4096

// scram signs a client in with SCRAM-SHA-256 (RFC 5802, RFC 7677) as a Postgres server does, the local token the
// password: the client proves it knows the token and never sends it, so a process listening on the remembered port
// while canyon db connect is not running learns nothing it can use. The only mechanism offered; false for anything
// else, a cleartext password included, and for a proof that does not hold.
func (l *dbListener) scram(be *pgproto3.Backend) bool {
	be.Send(&pgproto3.AuthenticationSASL{AuthMechanisms: []string{"SCRAM-SHA-256"}})
	if be.Flush() != nil || be.SetAuthType(pgproto3.AuthTypeSASL) != nil {
		return false
	}
	msg, err := be.Receive()
	first, ok := msg.(*pgproto3.SASLInitialResponse)
	if err != nil || !ok || first.AuthMechanism != "SCRAM-SHA-256" {
		return false
	}
	// client-first-message: a gs2 header of n or y (no channel binding is offered), no authzid, then n= and r=
	data := string(first.Data)
	var header string
	for _, h := range []string{"n,,", "y,,"} {
		if strings.HasPrefix(data, h) {
			header = h
		}
	}
	bare := strings.TrimPrefix(data, header)
	fields := strings.Split(bare, ",")
	if header == "" || len(fields) < 2 || !strings.HasPrefix(fields[0], "n=") || !strings.HasPrefix(fields[1], "r=") || len(fields[1]) < 3 {
		return false
	}
	ours, salt := make([]byte, 18), make([]byte, 16)
	if _, err := rand.Read(ours); err != nil {
		return false
	}
	if _, err := rand.Read(salt); err != nil {
		return false
	}
	nonce := fields[1][2:] + base64.RawStdEncoding.EncodeToString(ours)
	serverFirst := "r=" + nonce + ",s=" + base64.StdEncoding.EncodeToString(salt) + ",i=4096"
	be.Send(&pgproto3.AuthenticationSASLContinue{Data: []byte(serverFirst)})
	if be.Flush() != nil || be.SetAuthType(pgproto3.AuthTypeSASLContinue) != nil {
		return false
	}
	if msg, err = be.Receive(); err != nil {
		return false
	}
	final, ok := msg.(*pgproto3.SASLResponse)
	if !ok {
		return false
	}
	// client-final-message: c= the header echoed, r= the whole nonce, then p= the proof, last
	without, encoded, found := strings.Cut(string(final.Data), ",p=")
	proof, err := base64.StdEncoding.DecodeString(encoded)
	if !found || err != nil || len(proof) != sha256.Size || without != "c="+base64.StdEncoding.EncodeToString([]byte(header))+",r="+nonce {
		return false
	}
	token, ok := l.signInToken()
	if !ok {
		return false
	}
	salted, err := pbkdf2.Key(sha256.New, token, salt, scramIterations, sha256.Size)
	if err != nil {
		return false
	}
	auth := []byte(bare + "," + serverFirst + "," + without)
	clientKey := scramHMAC(salted, []byte("Client Key"))
	stored := sha256.Sum256(clientKey)
	signature := scramHMAC(stored[:], auth)
	for i := range proof { // the client's key, if the proof holds
		proof[i] ^= signature[i]
	}
	if got := sha256.Sum256(proof); subtle.ConstantTimeCompare(got[:], stored[:]) != 1 {
		return false
	}
	be.Send(&pgproto3.AuthenticationSASLFinal{Data: []byte("v=" + base64.StdEncoding.EncodeToString(scramHMAC(scramHMAC(salted, []byte("Server Key")), auth)))})
	return be.Flush() == nil
}

func scramHMAC(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}
