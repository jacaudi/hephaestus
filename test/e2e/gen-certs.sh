#!/bin/sh
# Generates the e2e test CA and a leaf for hephaestus.e2e.test into /certs. Runs inside the toolbox image.
# RSA-2048, with notBefore back-dated so clock skew between host and VM cannot reject the chain.
set -eu
cd /certs
openssl req -x509 -newkey rsa:2048 -nodes -keyout ca.key -out ca.pem -days 30 \
	-subj "/CN=hephaestus e2e test CA" -not_before 20261101000000Z \
	-addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign"
openssl req -newkey rsa:2048 -nodes -keyout privkey.pem -out leaf.csr -subj "/CN=hephaestus.e2e.test"
printf 'subjectAltName=DNS:hephaestus.e2e.test\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n' >leaf.ext
openssl x509 -req -in leaf.csr -CA ca.pem -CAkey ca.key -CAcreateserial -out leaf.pem -days 30 \
	-not_before 20261101000000Z -extfile leaf.ext
cat leaf.pem ca.pem >fullchain.pem
chmod 0644 ./*.pem ca.key # readable by UID 65532 through a :ro mount; throwaway test keys
