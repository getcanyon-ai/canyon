// Package subjecthash is OpenSSL's subject-name hash, the <8 hex> of the <8 hex>.<n> file names an OpenSSL capath
// directory and Paketo's JVM certificate loader find a CA under (design/EGRESS.md §4.4; contracts/executor.md,
// egress.ca): X509_NAME_hash_ex, the SHA-1 of the name's canonical encoding, its first four bytes little-endian.
package subjecthash

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // OpenSSL's name hash is SHA-1: a file name, never a signature or a check of trust
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"unicode/utf16"
	"unicode/utf8"
)

// The ASN.1 string types OpenSSL canonicalises (ASN1_MASK_CANON); any other is encoded as it came.
const (
	tagUTF8      = 12
	tagPrintable = 19
	tagT61       = 20
	tagIA5       = 22
	tagVisible   = 26
	tagUniversal = 28
	tagBMP       = 30
)

type attribute struct {
	Type  asn1.ObjectIdentifier
	Value asn1.RawValue
}

// Of is the hash of rawName, a Name's DER (an x509.Certificate's RawSubject), as OpenSSL writes it: 8 lower-case hex.
func Of(rawName []byte) (string, error) {
	var rdns []asn1.RawValue
	if rest, err := asn1.Unmarshal(rawName, &rdns); err != nil || len(rest) != 0 {
		return "", errors.New("subjecthash: not one DER Name")
	}
	var canon []byte
	for _, rdn := range rdns {
		var attrs []attribute
		if rest, err := asn1.UnmarshalWithParams(rdn.FullBytes, &attrs, "set"); err != nil || len(rest) != 0 || len(attrs) == 0 {
			return "", errors.New("subjecthash: a relative distinguished name that is not a SET of attributes")
		}
		var encoded [][]byte
		for _, a := range attrs {
			v, err := canonical(a.Value)
			if err != nil {
				return "", err
			}
			b, err := asn1.Marshal(attribute{a.Type, v})
			if err != nil {
				return "", err
			}
			encoded = append(encoded, b)
		}
		slices.SortFunc(encoded, bytes.Compare) // DER's SET OF order
		set, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true, Bytes: bytes.Join(encoded, nil)})
		if err != nil {
			return "", err
		}
		canon = append(canon, set...) // each SET, with no outer SEQUENCE: x509_name_canon
	}
	sum := sha1.Sum(canon) //nolint:gosec // see the import
	return fmt.Sprintf("%08x", binary.LittleEndian.Uint32(sum[:4])), nil
}

// canonical is a value as asn1_string_canon makes it: a string type it knows, as UTF-8, its leading and trailing
// white space dropped, each run of it inside one space, its ASCII letters lower-cased, as a UTF8String; any other
// value as it came.
func canonical(v asn1.RawValue) (asn1.RawValue, error) {
	if v.Class != asn1.ClassUniversal || v.IsCompound {
		return v, nil
	}
	var text []byte
	switch v.Tag {
	case tagUTF8, tagPrintable, tagIA5, tagVisible:
		text = v.Bytes
	case tagT61: // OpenSSL reads a T61String as one byte a character, Latin-1
		for _, b := range v.Bytes {
			text = utf8.AppendRune(text, rune(b))
		}
	case tagBMP:
		if len(v.Bytes)%2 != 0 {
			return v, errors.New("subjecthash: a BMPString of an odd length")
		}
		units := make([]uint16, len(v.Bytes)/2)
		for i := range units {
			units[i] = binary.BigEndian.Uint16(v.Bytes[2*i:])
		}
		for _, r := range utf16.Decode(units) {
			text = utf8.AppendRune(text, r)
		}
	case tagUniversal:
		if len(v.Bytes)%4 != 0 {
			return v, errors.New("subjecthash: a UniversalString of a length not a multiple of 4")
		}
		for i := 0; i < len(v.Bytes); i += 4 {
			c := binary.BigEndian.Uint32(v.Bytes[i:])
			if c > utf8.MaxRune {
				return v, errors.New("subjecthash: a UniversalString past Unicode")
			}
			text = utf8.AppendRune(text, rune(c))
		}
	default:
		return v, nil
	}
	return asn1.RawValue{Class: asn1.ClassUniversal, Tag: tagUTF8, Bytes: fold(text)}, nil
}

// fold is OpenSSL's: white space at either end dropped, each run inside one space, ASCII lower-cased, any other
// byte copied.
func fold(s []byte) []byte {
	space := func(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\v' || b == '\f' || b == '\r' }
	for len(s) > 0 && space(s[0]) {
		s = s[1:]
	}
	for len(s) > 0 && space(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch b := s[i]; {
		case b >= 0x80:
			out = append(out, b)
		case space(b):
			out = append(out, ' ')
			for i+1 < len(s) && space(s[i+1]) {
				i++
			}
		case 'A' <= b && b <= 'Z':
			out = append(out, b+'a'-'A')
		default:
			out = append(out, b)
		}
	}
	return out
}
