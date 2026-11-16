# iPXE: a TLS 1.3 NewSessionTicket sent before the client Finished corrupts the handshake transcript

| | |
|---|---|
| Project | iPXE (`github.com/ipxe/ipxe`) |
| Affected | commit `9261ebe38af0c0b38ac332adeec125a518845013` (`iPXE 2.0.0+ (g9261e)`), with TLS 1.3 enabled by default since `a040f9579` |
| Found | 2026-11-16, by the hephaestus e2e spike (QEMU OVMF/SeaBIOS/AAVMF on colima) |
| Status | **Not filed upstream** (owner decision, 2026-11-16). Recorded here so it can be filed later. |
| hephaestus workaround | `SessionTicketsDisabled: true` on the boot HTTPS listener (commit `275f131`); proxy mode needs the same at the proxy (Caddy: `apps.tls.session_tickets.disabled`). Decision D26 in `docs/research/booty-slim/README.md`. |

## Symptom

An iPXE client cannot complete a TLS 1.3 handshake with a Go `crypto/tls` server that has session tickets enabled (Go's default).

- The first attempt ends with the server rejecting the client Finished. In Go's log: `http: TLS handshake error from <ip>: tls: invalid client finished hash` (alert 51, `decrypt_error`).
- Every retry offers the ticket from the failed handshake as a PSK and fails with `tls: invalid PSK binder`.
- On the iPXE console the download fails with error `410de18f`. The embedded script exhausts its retries and returns to the firmware.

The certificate and CA are not involved. The same thing happens behind Caddy with its default session tickets on.

## Evidence (wire)

Decrypted with a Go `KeyLogWriter` key log and tshark:

1. ClientHello (iPXE): `supported_versions` 0x0304, 0x0303, 0x0302; groups x25519, secp256r1, secp384r1, ffdhe2048/3072/4096; one key share (x25519); TLS 1.3 suites 0x1301, 0x1302.
2. ServerHello (Go): no HelloRetryRequest; x25519; `TLS_AES_128_GCM_SHA256` (0x1301).
3. The server's first flight is one segment of handshake types `2, 8, 11, 15, 20, 4`: ServerHello, EncryptedExtensions, Certificate, CertificateVerify, Finished, **NewSessionTicket**. Go sends its tickets right after its own Finished, before it reads the client Finished.
4. Client Finished → server alert 51.
5. With `SessionTicketsDisabled: true` the flight is `2, 8, 11, 15, 20`. The handshake completes, and `/boot` and `/discover` return 200.

HelloRetryRequest and the cipher suite were ruled out. Restricting Go to X25519/P-256 changed nothing, and the negotiated suite is SHA-256.

## Root cause

`src/net/tls.c` at 9261ebe, in the handshake dispatch loop (lines 3647–3699):

```c
		case TLS_NEW_SESSION_TICKET:
			handler = tls_new_session_ticket;
			break;
		...
		/* Add to transcript digest (except for Hello Requests,
		 * which are explicitly excluded).
		 */
		if ( handshake->type != TLS_HELLO_REQUEST )
			tls_add_handshake ( tls, handshake, len );

		/* Freeze transcript digest if handshake has completed */
		if ( tls_ready ( tls ) )
			tlskey_freeze ( &tls->key );
```

Every handshake message except HelloRequest is added to the transcript digest until the handshake is ready. When a NewSessionTicket arrives before iPXE has sent its own Finished, the ticket is hashed into the transcript. iPXE's client Finished, and the resumption secret derived from that transcript, are then wrong. That accounts for both the `invalid client finished hash` and the later `invalid PSK binder`. `tls_new_session_ticket()` (line 3130) also keeps the ticket for resumption.

RFC 8446 classes NewSessionTicket as a **post-handshake message** (§4.6). The transcript hash (§4.4.1) covers the handshake messages only. §4.6.1 explicitly allows a server that does not request client authentication to send NewSessionTicket immediately after its own Finished, without waiting for the client's. Go's behaviour is therefore conformant, and the defect is in iPXE.

How the evidence was obtained: the cause is inferred from the source above and confirmed by an A/B test. The only variable was server-side session tickets. Off, every firmware boots; on, every boot fails.

## Suggested upstream fix

In TLS 1.3, never add a NewSessionTicket (or any other post-handshake message such as KeyUpdate) to the transcript digest. Skip `tls_add_handshake()` for those types, as is already done for HelloRequest. Process the ticket only for resumption, after the client Finished has been computed. A regression test would be a TLS 1.3 server that sends NewSessionTicket in the same flight as its Finished.

## Reproduce

From the hephaestus repo, on colima or any Linux Docker daemon:

1. In a temporary copy, set `SessionTicketsDisabled: false` on the boot HTTPS `tls.Config` in `cmd/hephaestus/serve.go`. In proxy mode, set it in `test/e2e/caddy.json`.
2. Run `task e2e`. Expected: the TLS and boot checks FAIL. The hephaestus or Caddy log shows `invalid client finished hash`, then `invalid PSK binder`.
3. With the shipped code, `task e2e` passes (71 checks).

## Not verified

- Whether OpenSSL-based servers avoid this by sending tickets after the client Finished. It is plausible but was not tested.
- Whether Traefik and Envoy send tickets early by default and so need the same setting. Not tested.
- Physical NIC firmware. The reproduction used QEMU's OVMF, SeaBIOS and AAVMF, chaining into the iPXE 9261ebe build.

## Evidence files

In the main checkout, under `.superpowers/sdd/2026-11-16-e2e-harness-implementation/`:

- `SPIKE.md`, the spike report ("Key finding");
- `progress.final.md`, the ledger.
