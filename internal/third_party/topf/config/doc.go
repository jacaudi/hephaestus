// hephaestus: new

// Package config loads a Topf cluster definition (topf.yaml and its patch folders) from an fs.FS.
//
// It is the pkg/config package of github.com/postfinance/topf at the commit named in ../VENDORED.md,
// changed to read from an fs.FS instead of the host file system and to substitute secret references from a
// map instead of piping files through the vals binary and SOPS. It never runs a process and never reads the
// host file system.
package config
