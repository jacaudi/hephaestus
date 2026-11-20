// Package render checks a Topf cluster definition imported from a git tree before anything renders it: the
// template-function allow-list, the grammar of a secret reference and the rules Load applies to a tree.
//
// A definition is untrusted input. Load reads it through an fs.FS, never the host file system, and refuses it with a
// *RefusalError that names the file and the rule, never a value of the file. The refusals follow the design's load rules
// (docs/designs/2026-11-18-hephaestus-m3-first-install-design.md, 5.5): no provider that runs a program, no path or link
// that leaves the tree, no reference that is not a plain 1Password one, no SOPS file, no template function that is not
// on the allow-list.
//
// The package reads the same files as the vendored Topf render (internal/third_party/topf) and applies Topf's own file
// selection, so a tree Load accepts is a tree the render reads in full.
package render
