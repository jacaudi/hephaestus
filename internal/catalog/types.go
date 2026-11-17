package catalog

import (
	"path"
	"slices"
	"strings"
)

// ArtifactType is the OCI artifact type of every catalog entry in the store.
const ArtifactType = "application/vnd.hephaestus.utility.v1"

// TagPrefix starts every catalog entry's tag in the store.
const TagPrefix = "utility/"

// Platform is the firmware a machine boots with.
type Platform string

// The supported platforms.
const (
	// PlatformEFI is UEFI firmware.
	PlatformEFI Platform = "efi"
	// PlatformPCBIOS is legacy BIOS firmware.
	PlatformPCBIOS Platform = "pcbios"
)

// ParsePlatform returns the Platform named s, and false for any other text.
func ParsePlatform(s string) (Platform, bool) {
	switch p := Platform(s); p {
	case PlatformEFI, PlatformPCBIOS:
		return p, true
	}
	return "", false
}

// Container is how an upstream object packs its files.
type Container string

// The container types.
const (
	// ContainerRaw is a single file, used as it is.
	ContainerRaw Container = "raw"
	// ContainerZip is a zip archive.
	ContainerZip Container = "zip"
	// ContainerISO is an ISO9660 image.
	ContainerISO Container = "iso"
)

// Recipe is the shape of the iPXE script an entry boots with.
type Recipe string

// The recipe kinds.
const (
	// RecipeChain chains one EFI binary.
	RecipeChain Recipe = "chain"
	// RecipeKernel loads a kernel with its arguments and boots it.
	RecipeKernel Recipe = "kernel"
	// RecipeArchiso boots an Arch Linux archiso image over HTTP.
	RecipeArchiso Recipe = "archiso"
	// RecipeLiveBoot boots a Debian live-boot image, fetching its squashfs.
	RecipeLiveBoot Recipe = "live-boot"
)

// HonoursBootIF reports whether the recipe's operating system uses the BOOTIF
// argument to pick the NIC it booted from. Chain and kernel recipes pass none.
func (r Recipe) HonoursBootIF() bool {
	return r == RecipeArchiso || r == RecipeLiveBoot
}

// Key identifies an entry for one architecture.
type Key struct{ Entry, Arch string }

// Signature says how an upstream object is authenticated at pin time. Either
// TOFU is set (trust on first use) or SigURL and Key are: a detached
// signature over the object, or over the checksum list ChecksumsURL names.
type Signature struct {
	// TOFU marks an object nothing upstream can authenticate.
	TOFU bool
	// SigURL is the detached signature.
	SigURL string
	// ChecksumsURL is an upstream checksum list the object must appear in.
	ChecksumsURL string
	// ChecksumAlgo is "sha256" or "sha1"; it is set exactly when ChecksumsURL is.
	ChecksumAlgo string
	// Key is the file name of the signer's key under keys/.
	Key string
}

// ObjectSpec is one upstream object and the files taken from it.
type ObjectSpec struct {
	// URL is the https location of the object.
	URL       string
	Container Container
	Signature Signature
	// Files are the paths extracted from the object.
	Files []string
}

// Boot names the files a recipe boots, each one of the spec's files.
type Boot struct {
	// Kernel is the file loaded or chained.
	Kernel string
	// Initrds are loaded after the kernel, in order.
	Initrds []string
	// Second is the second-stage image (the squashfs) the OS fetches itself.
	Second string
}

// Spec is the hand-written description of one entry for one architecture.
type Spec struct {
	Entry, Arch, Version string
	Recipe               Recipe
	Platforms            []Platform
	Objects              []ObjectSpec
	Boot                 Boot
	// Args are the kernel arguments the recipe adds.
	Args string
	// Warning is shown to an admin who selects the entry.
	Warning string
}

// Key returns the spec's entry and architecture.
func (s Spec) Key() Key { return Key{Entry: s.Entry, Arch: s.Arch} }

// Supports reports whether the spec lists p.
func (s Spec) Supports(p Platform) bool { return slices.Contains(s.Platforms, p) }

// FilePin is the generated record of one extracted file.
type FilePin struct {
	Path   string
	Size   int64
	SHA256 string
	// EFIStub records that the kernel is bootable as an EFI binary.
	EFIStub bool
}

// ObjectPin is the generated record of one upstream object.
type ObjectPin struct {
	URL    string
	Size   int64
	SHA256 string
	// Signer is the fingerprint that verified the object; empty for TOFU.
	Signer string
	Files  []FilePin
}

// Pin is the generated record that fixes an entry's bytes.
type Pin struct {
	// ID is PinID(Objects).
	ID      string
	Objects []ObjectPin
}

// Entry is a spec together with its pin.
type Entry struct {
	Spec

	Pin Pin
}

// Tag returns the entry's tag in the store: TagPrefix, entry, "-", arch, ":"
// and the pin id.
func (e Entry) Tag() string {
	return TagPrefix + e.Entry + "-" + e.Arch + ":" + e.Pin.ID
}

// File returns the pin of the file at p, and false when no object holds it.
func (e Entry) File(p string) (FilePin, bool) {
	for _, o := range e.Pin.Objects {
		for _, f := range o.Files {
			if f.Path == p {
				return f, true
			}
		}
	}
	return FilePin{}, false
}

// TOFU reports whether any of the entry's objects is trust on first use.
func (e Entry) TOFU() bool {
	return slices.ContainsFunc(e.Objects, func(o ObjectSpec) bool { return o.Signature.TOFU })
}

const (
	// ramHeadroom is the memory a booted OS needs beyond its image.
	ramHeadroom = 512 << 20
	// ramStep is the granularity minimum RAM is rounded up to.
	ramStep = 256 << 20
)

// MinRAMBytes returns the memory the entry needs: its files' sizes plus
// 512 MiB, rounded up to a multiple of 256 MiB. It is a display value.
func (e Entry) MinRAMBytes() int64 {
	total := int64(ramHeadroom)
	for _, o := range e.Pin.Objects {
		for _, f := range o.Files {
			total += f.Size
		}
	}
	return (total + ramStep - 1) / ramStep * ramStep
}

// ValidPath reports whether p names a file by a clean relative path with no
// ".." element.
func ValidPath(p string) bool {
	if p == "" || p == "." || path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	return !slices.Contains(strings.Split(p, "/"), "..")
}
