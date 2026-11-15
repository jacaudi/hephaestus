// Package tlscert serves a certificate and key that something outside
// hephaestus writes and renews, such as certbot, lego or cert-manager. A Loader
// reads the pair once at startup, refuses one that does not match, does not
// cover the service's name or has expired, then re-reads the files when they
// change and serves the new pair to the TLS listener through GetCertificate.
// It never obtains a certificate itself.
package tlscert
