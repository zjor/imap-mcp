#!/bin/sh
# Generates a test CA and a server certificate for the Dovecot container.
# Output: certs/{ca.pem,server.pem,server.key} next to this script (git-ignored).
set -eu
dir="$(cd "$(dirname "$0")" && pwd)/certs"
mkdir -p "$dir"
cd "$dir"

openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
	-keyout ca.key -out ca.pem -subj "/CN=imap-mcp test CA" 2>/dev/null

openssl req -newkey rsa:2048 -nodes \
	-keyout server.key -out server.csr -subj "/CN=localhost" 2>/dev/null

printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\nbasicConstraints=CA:FALSE\nextendedKeyUsage=serverAuth\n' > server.ext
openssl x509 -req -in server.csr -CA ca.pem -CAkey ca.key -CAcreateserial \
	-days 3650 -out server.pem -extfile server.ext 2>/dev/null

rm -f server.csr server.ext ca.srl
chmod 644 server.key
echo "certs written to $dir"
