# TLS

This page explains how the boot HTTPS service gets its certificate, and how to set up each of the two modes (`file` and `proxy`).

hephaestus never obtains a certificate. The iPXE binaries chain to `https://<HEPHAESTUS_FQDN>/boot` and trust only the ISRG roots, so the certificate must chain to them (Let's Encrypt's default chain does). `HEPHAESTUS_TLS_MODE` says who holds it. The variables are listed in [Configuration](configuration.md).

## File mode

This is the default. hephaestus serves a chain and key that another process writes and renews: certbot, lego, cert-manager. The certificate file must hold the full chain (`fullchain.pem`, not `cert.pem`) and the chain must reach one of the roots the embedded iPXE trusts (the four ISRG roots). It reads both files at startup and refuses to start when either is unreadable, the key does not match, the leaf does not cover `HEPHAESTUS_FQDN`, the leaf has expired, or the chain does not reach one of those roots, so a chain that does not reach those roots never reports ready. Once a minute it looks at both files, following symlinks, so a Kubernetes secret update counts. When one changed it reloads the pair under the same checks; a bad replacement, an untrusted chain included, is logged as a warning and the current pair keeps serving. The admin listener reports `tls:certificate` (critical; fails after the leaf expires) and `tls:expiry` (warns under 14 days). Mount the directory into the image read-only, readable by UID 65532:

```sh
docker run ... -v /etc/hephaestus/certs:/certs:ro \
  -e HEPHAESTUS_TLS_CERT_FILE=/certs/fullchain.pem -e HEPHAESTUS_TLS_KEY_FILE=/certs/privkey.pem ...
```

## Proxy mode

A reverse proxy (Caddy, Traefik, Envoy) terminates TLS 1.3 with a certificate that chains to the ISRG roots, and forwards plain HTTP to `HEPHAESTUS_PROXY_ADDRESS`. hephaestus then serves `/boot`, `/discover` and `/tftp` there and binds neither the HTTPS port nor any certificate. The proxy must replace or append to a client-supplied `X-Forwarded-For`, never pass it through, because hephaestus believes the header from a trusted peer and a passed-through one lets the client name any address. Caddy and Traefik do this by default. Envoy does not: its default (`use_remote_address: false`) leaves the header untouched, so set `use_remote_address: true` with `xff_num_trusted_hops: 0`. The proxy's own address must be in `HEPHAESTUS_PROXY_TRUSTED`. hephaestus reads the client address from the rightmost `X-Forwarded-For` entry that is not a trusted proxy, and answers 403 to any peer outside the trusted list and to a request with no usable header. Point `HEPHAESTUS_FQDN`'s DNS record at the proxy. Plain HTTP `:80` (`/start`, `/boot` as a chain to HTTPS, `/tftp`, `/u/`) stays on hephaestus, because the embedded chain script targets the next-server address, not the name.

The proxy must also disable TLS session tickets: iPXE 9261ebe hashes a NewSessionTicket that arrives before its own Finished into the handshake transcript, so a proxy that sends one early (Go's TLS stack does) fails the handshake. The full finding is in [the iPXE transcript note](upstream/ipxe-tls13-new-session-ticket-transcript.md). Caddy's Caddyfile has no option for this, so the example is Caddy's JSON config (`apps.tls.session_tickets.disabled`); a Caddyfile user converts theirs with `caddy adapt` and then adds that key. Traefik and Envoy were not tested; check theirs.

```json
{
	"apps": {
		"http": {"servers": {"boot": {
			"listen": [":443"],
			"tls_connection_policies": [{"protocol_min": "tls1.3"}],
			"routes": [{
				"match": [{"host": ["boot.example.com"]}],
				"handle": [{"handler": "reverse_proxy", "upstreams": [{"dial": "127.0.0.1:8443"}]}]
			}]
		}}},
		"tls": {"session_tickets": {"disabled": true}}
	}
}
```

`127.0.0.1:8443` is `HEPHAESTUS_PROXY_ADDRESS`; the proxy's address is in `HEPHAESTUS_PROXY_TRUSTED`.

[← Back to README](../README.md)
