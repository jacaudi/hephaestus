// Package catalog is the compiled-in list of utility operating systems the
// server can boot a machine into: what each one is, where it comes from, how
// its iPXE script is written, and the pin that fixes the bytes it must be.
// Hand-written fields live in catalog.go and generated pins in
// catalog_pins.go. The package is data and script rendering only; it does no
// I/O and imports no other hephaestus package outside its tests.
package catalog
