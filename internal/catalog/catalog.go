package catalog

// specs is the hand-written half of the catalog: one Spec per entry and
// architecture, from design §5.2 and the pin spike. Sizes, hashes, extracted
// paths and pin ids are generated into catalog_pins.go. Every URL is https.
var specs = []Spec{ //nolint:gochecknoglobals // compiled-in catalog data (design §5.2)
	{
		Entry: "memtest86plus", Arch: "amd64", Version: "9.00",
		Recipe:    RecipeChain,
		Platforms: []Platform{PlatformEFI},
		Objects: []ObjectSpec{{
			URL:       "https://www.memtest.org/download/v9.00/mt86plus_9.00.binaries.zip",
			Container: ContainerZip,
			Signature: Signature{TOFU: true, ChecksumsURL: "https://www.memtest.org/download/v9.00/sha256sum.txt", ChecksumAlgo: "sha256"},
			Files:     []string{"mt86p_900_x86_64"},
		}},
		Boot: Boot{Kernel: "mt86p_900_x86_64"},
	},
	{
		Entry: "memtest86plus", Arch: "arm64", Version: "9.00",
		Recipe:    RecipeChain,
		Platforms: []Platform{PlatformEFI},
		Objects: []ObjectSpec{{
			URL:       "https://www.memtest.org/download/v9.00/mt86plus_9.00.binaries.zip",
			Container: ContainerZip,
			Signature: Signature{TOFU: true, ChecksumsURL: "https://www.memtest.org/download/v9.00/sha256sum.txt", ChecksumAlgo: "sha256"},
			Files:     []string{"mt86p_900_arm64"},
		}},
		Boot: Boot{Kernel: "mt86p_900_arm64"},
	},
	{
		Entry: "systemrescue", Arch: "amd64", Version: "13.03",
		Recipe:    RecipeArchiso,
		Platforms: []Platform{PlatformPCBIOS, PlatformEFI},
		Objects: []ObjectSpec{{
			URL:       "https://fastly-cdn.system-rescue.org/releases/13.03/systemrescue-13.03-amd64.iso",
			Container: ContainerISO,
			Signature: Signature{SigURL: "https://www.system-rescue.org/releases/13.03/systemrescue-13.03-amd64.iso.asc", Key: "systemrescue.asc"},
			Files: []string{
				"sysresccd/boot/x86_64/vmlinuz",
				"sysresccd/boot/intel_ucode.img",
				"sysresccd/boot/amd_ucode.img",
				"sysresccd/boot/x86_64/sysresccd.img",
				"sysresccd/x86_64/airootfs.sfs",
				"sysresccd/x86_64/airootfs.sha512",
			},
		}},
		Boot: Boot{
			Kernel: "sysresccd/boot/x86_64/vmlinuz",
			Initrds: []string{
				"sysresccd/boot/intel_ucode.img",
				"sysresccd/boot/amd_ucode.img",
				"sysresccd/boot/x86_64/sysresccd.img",
			},
			Second: "sysresccd/x86_64/airootfs.sfs",
		},
		Args: "archisobasedir=sysresccd",
	},
	{
		Entry: "clonezilla", Arch: "amd64", Version: "3.3.3-37",
		Recipe:    RecipeLiveBoot,
		Platforms: []Platform{PlatformPCBIOS, PlatformEFI},
		Objects: []ObjectSpec{{
			URL:       "https://downloads.sourceforge.net/clonezilla/clonezilla-live-3.3.3-37-amd64.zip",
			Container: ContainerZip,
			Signature: Signature{
				ChecksumsURL: "https://clonezilla.org/downloads/stable/data/CHECKSUMS.TXT",
				ChecksumAlgo: "sha256",
				SigURL:       "https://clonezilla.org/downloads/stable/data/CHECKSUMS.TXT.gpg",
				Key:          "clonezilla.asc",
			},
			Files: []string{"live/vmlinuz", "live/initrd.img", "live/filesystem.squashfs"},
		}},
		Boot: Boot{Kernel: "live/vmlinuz", Initrds: []string{"live/initrd.img"}, Second: "live/filesystem.squashfs"},
		Args: "boot=live union=overlay username=user config components noswap edd=on nomodeset net.ifnames=0 noprompt ocs_live_run=ocs-live-general ocs_live_batch=no",
	},
	{
		Entry: "gparted", Arch: "amd64", Version: "1.8.1-6",
		Recipe:    RecipeLiveBoot,
		Platforms: []Platform{PlatformPCBIOS, PlatformEFI},
		Objects: []ObjectSpec{{
			URL:       "https://downloads.sourceforge.net/gparted/gparted-live-1.8.1-6-amd64.zip",
			Container: ContainerZip,
			Signature: Signature{
				ChecksumsURL: "https://gparted.org/gparted-live/stable/CHECKSUMS.TXT",
				ChecksumAlgo: "sha256",
				SigURL:       "https://gparted.org/gparted-live/stable/CHECKSUMS.TXT.gpg",
				Key:          "gparted.asc",
			},
			Files: []string{"live/vmlinuz", "live/initrd.img", "live/filesystem.squashfs"},
		}},
		Boot: Boot{Kernel: "live/vmlinuz", Initrds: []string{"live/initrd.img"}, Second: "live/filesystem.squashfs"},
		Args: "boot=live union=overlay username=user config components noswap noeject",
	},
	{
		Entry: "shredos", Arch: "amd64", Version: "v2025.11_32_x86-64_0.43.1",
		Recipe:    RecipeKernel,
		Platforms: []Platform{PlatformPCBIOS, PlatformEFI},
		Objects: []ObjectSpec{{
			URL:       "https://github.com/PartialVolume/shredos.x86_64/releases/download/v2025.11_32_x86-64_0.43.1/shredos-2025.11_32_x86-64_v0.43.1_20261010.iso",
			Container: ContainerISO,
			Signature: Signature{
				TOFU:         true,
				ChecksumsURL: "https://github.com/PartialVolume/shredos.x86_64/releases/download/v2025.11_32_x86-64_0.43.1/shredos-2025.11_32_x86-64_v0.43.1_20261010.iso.sha1",
				ChecksumAlgo: "sha1",
			},
			Files: []string{"boot/bzImage"},
		}},
		Boot:    Boot{Kernel: "boot/bzImage"},
		Args:    "console=tty3 loglevel=3",
		Warning: "ShredOS erases disks. It boots interactive and erases nothing until someone at the console starts a wipe.",
	},
	{
		Entry: "grml", Arch: "amd64", Version: "2026.09",
		Recipe:    RecipeLiveBoot,
		Platforms: []Platform{PlatformPCBIOS, PlatformEFI},
		Objects: []ObjectSpec{{
			URL:       "https://ftp-master.grml.org/grml-full-2026.09-amd64.iso",
			Container: ContainerISO,
			Signature: Signature{
				ChecksumsURL: "https://ftp-master.grml.org/SHA256SUMS-2026.09",
				ChecksumAlgo: "sha256",
				SigURL:       "https://ftp-master.grml.org/SHA256SUMS-2026.09.gpg",
				Key:          "grml.asc",
			},
			Files: []string{
				"boot/grmlfullamd64/vmlinuz",
				"boot/grmlfullamd64/initrd.img",
				"live/grml-full-amd64/grml-full-amd64.squashfs",
			},
		}},
		Boot: Boot{
			Kernel:  "boot/grmlfullamd64/vmlinuz",
			Initrds: []string{"boot/grmlfullamd64/initrd.img"},
			Second:  "live/grml-full-amd64/grml-full-amd64.squashfs",
		},
		Args: "boot=live",
	},
	{
		Entry: "grml", Arch: "arm64", Version: "2026.09",
		Recipe:    RecipeLiveBoot,
		Platforms: []Platform{PlatformEFI},
		Objects: []ObjectSpec{{
			URL:       "https://ftp-master.grml.org/grml-full-2026.09-arm64.iso",
			Container: ContainerISO,
			Signature: Signature{
				ChecksumsURL: "https://ftp-master.grml.org/SHA256SUMS-2026.09",
				ChecksumAlgo: "sha256",
				SigURL:       "https://ftp-master.grml.org/SHA256SUMS-2026.09.gpg",
				Key:          "grml.asc",
			},
			Files: []string{
				"boot/grmlfullarm64/vmlinuz",
				"boot/grmlfullarm64/initrd.img",
				"live/grml-full-arm64/grml-full-arm64.squashfs",
			},
		}},
		Boot: Boot{
			Kernel:  "boot/grmlfullarm64/vmlinuz",
			Initrds: []string{"boot/grmlfullarm64/initrd.img"},
			Second:  "live/grml-full-arm64/grml-full-arm64.squashfs",
		},
		Args: "boot=live",
	},
	{
		Entry: "uefishell", Arch: "amd64", Version: "edk2-stable202002",
		Recipe:    RecipeChain,
		Platforms: []Platform{PlatformEFI},
		Objects: []ObjectSpec{{
			URL:       "https://github.com/tianocore/edk2/releases/download/edk2-stable202002/ShellBinPkg.zip",
			Container: ContainerZip,
			Signature: Signature{TOFU: true},
			Files:     []string{"ShellBinPkg/UefiShell/X64/Shell.efi"},
		}},
		Boot: Boot{Kernel: "ShellBinPkg/UefiShell/X64/Shell.efi"},
	},
	{
		Entry: "uefishell", Arch: "arm64", Version: "edk2-stable202002",
		Recipe:    RecipeChain,
		Platforms: []Platform{PlatformEFI},
		Objects: []ObjectSpec{{
			URL:       "https://github.com/tianocore/edk2/releases/download/edk2-stable202002/ShellBinPkg.zip",
			Container: ContainerZip,
			Signature: Signature{TOFU: true},
			Files:     []string{"ShellBinPkg/UefiShell/AArch64/Shell.efi"},
		}},
		Boot: Boot{Kernel: "ShellBinPkg/UefiShell/AArch64/Shell.efi"},
	},
}
