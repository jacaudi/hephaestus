// Package tlsacme keeps the service's TLS certificate current. A Manager
// loads the certificate stored under the data key, asks an Issuer for a new
// one when two thirds of its lifetime have passed, and serves it to the TLS
// listener through GetCertificate. LegoIssuer is the Let's Encrypt Issuer: it
// proves control of the domain with the DNS-01 challenge on Cloudflare. The
// Repository stores the ACME account and the certificate, both sealed.
package tlsacme
