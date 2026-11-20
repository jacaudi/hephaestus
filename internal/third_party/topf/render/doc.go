// hephaestus: new

// Package render renders the Talos machine configuration of every node of a Topf cluster definition, in process.
//
// It is the per-node render of github.com/postfinance/topf at the commit named in ../VENDORED.md (internal/topf's
// node and nodes, the secrets bundle load, and the schematic resolver), cut down to what an offline render needs:
// no live node is queried, no image factory is called, no process is run and no file outside the definition tree is
// read. The definition arrives as an fs.FS (see package config), the secret references as a map.
package render
