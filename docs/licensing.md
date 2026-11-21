# Licensing of Bundled Code

This page explains the copyleft code that the hephaestus image contains and what that means for publishing an image.

The image contains code under copyleft licences:

- Modified GPLv2 iPXE binaries, built from iPXE commit `9261ebe` with the build config in `build/ipxe/`.
- Modified MPL-2.0 files, vendored under `internal/third_party/omniprovider/` (see its `VENDORED.md`).
- MPL-2.0 files from image-factory (the Talos schematic model), vendored under `internal/third_party/imagefactory/` (see its `VENDORED.md`). `schematic.go` is unchanged; the files beside it are new.

Publishing an image therefore needs a public source repository or an accompanying source offer.

The image also contains MIT-licensed code that is vendored, not copyleft: the Topf cluster loader and per-node render, under `internal/third_party/topf/` (see its `VENDORED.md`, which records the source commit and every change). It keeps its licence file and copyright notice.

[← Back to README](../README.md)
