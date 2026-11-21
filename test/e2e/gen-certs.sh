#!/bin/sh
# Generates the e2e test CA and five leaves into /certs: hephaestus.e2e.test (privkey.pem, fullchain.pem),
# upstream.e2e.test, the fake upstream's (upstream-privkey.pem, upstream-fullchain.pem), and the three names the
# upstreams program serves: factory.e2e.test (factory-*), assets.factory.e2e.test (assets-*) and git.e2e.test (git-*).
# Runs inside the toolbox image.
# RSA-2048, with notBefore back-dated so clock skew between host and VM cannot reject the chain.
set -eu
cd /certs
openssl req -x509 -newkey rsa:2048 -nodes -keyout ca.key -out ca.pem -days 30 \
	-subj "/CN=hephaestus e2e test CA" -not_before 20261101000000Z \
	-addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign"

# issue <name> <key-file> <chain-file>: a leaf for <name> signed by the test CA; <chain-file> is the leaf then the CA.
issue() {
	openssl req -newkey rsa:2048 -nodes -keyout "$2" -out "$1.csr" -subj "/CN=$1"
	printf 'subjectAltName=DNS:%s\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n' "$1" >"$1.ext"
	openssl x509 -req -in "$1.csr" -CA ca.pem -CAkey ca.key -CAcreateserial -out "$1.pem" -days 30 \
		-not_before 20261101000000Z -extfile "$1.ext"
	cat "$1.pem" ca.pem >"$3"
}
issue hephaestus.e2e.test privkey.pem fullchain.pem
issue upstream.e2e.test upstream-privkey.pem upstream-fullchain.pem
issue factory.e2e.test factory-privkey.pem factory-fullchain.pem
issue assets.factory.e2e.test assets-privkey.pem assets-fullchain.pem
issue git.e2e.test git-privkey.pem git-fullchain.pem
chmod 0644 ./*.pem ca.key # readable by UID 65532 through a :ro mount; throwaway test keys
