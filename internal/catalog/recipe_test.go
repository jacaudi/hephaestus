package catalog_test

import (
	"net/netip"
	"strings"
	"testing"

	"go.jacaudi.dev/hephaestus/internal/catalog"
)

// bootIF is the one non-server substitution a script may carry.
const bootIF = "${net${idx}/mac:hexhyp}"

func testAddr(t *testing.T) netip.Addr {
	t.Helper()
	return netip.MustParseAddr("10.10.20.5")
}

// production returns the compiled-in spec for key.
func production(t *testing.T, entry, arch string) catalog.Spec {
	t.Helper()
	specs := catalog.Specs()
	for i := range specs {
		if specs[i].Entry == entry && specs[i].Arch == arch {
			return specs[i]
		}
	}
	t.Fatalf("no spec for %s %s", entry, arch)
	return catalog.Spec{}
}

func TestScript_Chain(t *testing.T) {
	got := production(t, "uefishell", "arm64").Script(testAddr(t))
	want := "#!ipxe\nchain http://10.10.20.5/u/uefishell/arm64/edk2-stable202002/ShellBinPkg/UefiShell/AArch64/Shell.efi\n"
	if got != want {
		t.Errorf("Script =\n%q\nwant\n%q", got, want)
	}
}

func TestScript_Archiso(t *testing.T) {
	got := production(t, "systemrescue", "amd64").Script(testAddr(t))
	want := "#!ipxe\nkernel http://10.10.20.5/u/systemrescue/amd64/13.03/sysresccd/boot/x86_64/vmlinuz initrd=intel_ucode.img initrd=amd_ucode.img initrd=sysresccd.img archisobasedir=sysresccd ip=dhcp BOOTIF=01-${net${idx}/mac:hexhyp} archiso_http_srv=http://10.10.20.5/u/systemrescue/amd64/13.03/ checksum\n" +
		"initrd http://10.10.20.5/u/systemrescue/amd64/13.03/sysresccd/boot/intel_ucode.img\n" +
		"initrd http://10.10.20.5/u/systemrescue/amd64/13.03/sysresccd/boot/amd_ucode.img\n" +
		"initrd http://10.10.20.5/u/systemrescue/amd64/13.03/sysresccd/boot/x86_64/sysresccd.img\n" +
		"boot\n"
	if got != want {
		t.Errorf("Script =\n%q\nwant\n%q", got, want)
	}
}

func TestScript_LiveBoot(t *testing.T) {
	got := production(t, "grml", "amd64").Script(testAddr(t))
	want := "#!ipxe\nkernel http://10.10.20.5/u/grml/amd64/2026.09/boot/grmlfullamd64/vmlinuz initrd=initrd.img boot=live BOOTIF=01-${net${idx}/mac:hexhyp} fetch=http://10.10.20.5/u/grml/amd64/2026.09/live/grml-full-amd64/grml-full-amd64.squashfs\n" +
		"initrd http://10.10.20.5/u/grml/amd64/2026.09/boot/grmlfullamd64/initrd.img\n" +
		"boot\n"
	if got != want {
		t.Errorf("Script =\n%q\nwant\n%q", got, want)
	}
}

func TestScript_Kernel(t *testing.T) {
	got := production(t, "shredos", "amd64").Script(testAddr(t))
	want := "#!ipxe\nkernel http://10.10.20.5/u/shredos/amd64/v2025.11_32_x86-64_0.43.1/boot/bzImage console=tty3 loglevel=3\nboot\n"
	if got != want {
		t.Errorf("Script =\n%q\nwant\n%q", got, want)
	}
}

func TestSpec_BaseURL(t *testing.T) {
	got := production(t, "grml", "arm64").BaseURL(testAddr(t))
	if want := "http://10.10.20.5/u/grml/arm64/2026.09/"; got != want {
		t.Errorf("BaseURL = %q, want %q", got, want)
	}
}

func TestScript_OnlyServerSubstitutions(t *testing.T) {
	for _, s := range catalog.Specs() {
		script := s.Script(testAddr(t))
		if rest := strings.ReplaceAll(script, bootIF, ""); strings.Contains(rest, "${") {
			t.Errorf("%v: script carries a substitution other than BOOTIF:\n%s", s.Key(), script)
		}
		if s.Recipe.HonoursBootIF() != strings.Contains(script, "BOOTIF=01-"+bootIF) {
			t.Errorf("%v: HonoursBootIF=%t disagrees with the script", s.Key(), s.Recipe.HonoursBootIF())
		}
	}
}

func TestRecipe_HonoursBootIF(t *testing.T) {
	tests := []struct {
		recipe catalog.Recipe
		want   bool
	}{
		{catalog.RecipeArchiso, true},
		{catalog.RecipeLiveBoot, true},
		{catalog.RecipeChain, false},
		{catalog.RecipeKernel, false},
	}
	for _, tt := range tests {
		if got := tt.recipe.HonoursBootIF(); got != tt.want {
			t.Errorf("%s.HonoursBootIF() = %t, want %t", tt.recipe, got, tt.want)
		}
	}
}

// TestScript_LineStructure pins the shape of every production script: the
// header, one kernel or chain line, the initrd lines, and boot where the
// recipe needs it. A line anywhere else is a script-injection hole.
func TestScript_LineStructure(t *testing.T) {
	specs := catalog.Specs()
	for i := range specs {
		s := &specs[i]
		script := s.Script(testAddr(t))
		if !strings.HasSuffix(script, "\n") || strings.Contains(script, "\r") {
			t.Errorf("%v: script does not end in one LF or carries a CR: %q", s.Key(), script)
			continue
		}
		lines := strings.Split(strings.TrimSuffix(script, "\n"), "\n")
		if lines[0] != "#!ipxe" {
			t.Errorf("%v: first line %q, want #!ipxe", s.Key(), lines[0])
		}
		if s.Recipe == catalog.RecipeChain {
			if len(lines) != 2 || !strings.HasPrefix(lines[1], "chain ") {
				t.Errorf("%v: chain script is %q, want a header and one chain line", s.Key(), lines)
			}
			continue
		}
		if len(lines) != 3+len(s.Boot.Initrds) {
			t.Errorf("%v: %d lines, want %d: %q", s.Key(), len(lines), 3+len(s.Boot.Initrds), lines)
			continue
		}
		if !strings.HasPrefix(lines[1], "kernel ") {
			t.Errorf("%v: second line %q, want a kernel line", s.Key(), lines[1])
		}
		for _, l := range lines[2 : len(lines)-1] {
			if !strings.HasPrefix(l, "initrd ") {
				t.Errorf("%v: line %q between kernel and boot is not an initrd", s.Key(), l)
			}
		}
		if last := lines[len(lines)-1]; last != "boot" {
			t.Errorf("%v: last line %q, want boot", s.Key(), last)
		}
	}
}
