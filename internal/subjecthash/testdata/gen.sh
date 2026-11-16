#!/bin/sh
# testdata/gen.sh <dir>: one self-signed CA per subject shape, and hashes.txt, each file's name and
# OpenSSL's own -subject_hash of it.
set -eu
out=$1
work=$(mktemp -d)
mkdir -p "$out"
: > "$out/hashes.txt"
make_ca() { # name mask multivalue subject
  cat > "$work/req.cnf" <<EOF
[req]
distinguished_name = dn
string_mask = $2
prompt = no
[dn]
[ca]
basicConstraints = critical,CA:true
keyUsage = critical,keyCertSign,cRLSign
EOF
  openssl ecparam -name prime256v1 -genkey -noout -out "$work/key.pem"
  if [ "$3" = yes ]; then mv=-multivalue-rdn; else mv=; fi
  openssl req -new -x509 -config "$work/req.cnf" -extensions ca -key "$work/key.pem" -days 3650 -utf8 $mv -subj "$4" -out "$out/$1.pem"
  printf '%s %s\n' "$1" "$(openssl x509 -in "$out/$1.pem" -noout -subject_hash)" >> "$out/hashes.txt"
}
make_ca utf8-spaces-case utf8only no '/O=Meridian  Insurance /CN=  Canyon   Egress ROOT '
make_ca printable default no '/C=GB/O=Meridian Ltd/CN=Canyon Root CA'
make_ca utf8-non-ascii utf8only no '/O=Zürich Ünïcode/CN=Ärger Root'
make_ca multivalue utf8only yes '/CN=Canyon+OU=Platform/O=Meridian'
make_ca email default no '/emailAddress=Ops@Meridian.Example/CN=Root'
make_ca t61-latin1 default no '/CN=Zürich ROOT'
make_ca bmp default no '/CN=日本 Canyon'
make_ca tabs utf8only no '/CN=Canyon	Tab		Root'
rm -rf "$work"
cat "$out/hashes.txt"
