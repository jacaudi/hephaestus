package catalog

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
)

// The architectures a spec may name. They are fleet.Arch's values; this
// package imports no other hephaestus package, and a test keeps them equal.
const (
	archAMD64 = "amd64"
	archARM64 = "arm64"
)

// Catalog is the validated set of entries: every spec that has a pin.
type Catalog struct {
	entries []Entry // sorted by entry, then arch
	byKey   map[Key]Entry
}

// Specs returns a copy of the compiled-in hand-written specs.
func Specs() []Spec {
	out := slices.Clone(specs)
	for i := range out {
		out[i] = out[i].clone()
	}
	return out
}

// Pins returns a copy of the compiled-in generated pins.
func Pins() map[Key]Pin {
	out := make(map[Key]Pin, len(pins))
	for k, p := range pins {
		out[k] = p.clone()
	}
	return out
}

// Absent returns a copy of the ledger of specs with no pin, each with its reason.
func Absent() map[Key]string { return maps.Clone(absent) }

// Default returns the catalog of the compiled-in specs and pins.
func Default() (*Catalog, error) { return New(specs, pins) }

// New returns the catalog of specs that have a pin in pins. It checks every
// spec, and every pin against its spec: the same URLs and file paths in the
// same order, sizes above zero, 64-digit hex sha256s, and an ID equal to
// PinID(Objects), and no two keys have one id. A pin with no spec is an error;
// a spec with no pin is left out. Every problem found is reported together.
func New(specs []Spec, pins map[Key]Pin) (*Catalog, error) {
	var errs []error
	bySpec := make(map[Key]Spec, len(specs))
	for i := range specs {
		s := &specs[i]
		if _, dup := bySpec[s.Key()]; dup {
			errs = append(errs, fmt.Errorf("spec %s/%s: listed twice", s.Entry, s.Arch))
			continue
		}
		bySpec[s.Key()] = *s
		if err := s.validate(); err != nil {
			errs = append(errs, fmt.Errorf("spec %s/%s: %w", s.Entry, s.Arch, err))
		}
	}

	c := &Catalog{byKey: make(map[Key]Entry, len(pins))}
	byPinID := make(map[string]Key, len(pins))
	for _, k := range slices.SortedFunc(maps.Keys(pins), compareKeys) {
		spec, ok := bySpec[k]
		if !ok {
			errs = append(errs, fmt.Errorf("pin %s/%s: no spec for it", k.Entry, k.Arch))
			continue
		}
		if err := validatePin(spec, pins[k]); err != nil {
			errs = append(errs, fmt.Errorf("pin %s/%s: %w", k.Entry, k.Arch, err))
			continue
		}
		// The fetcher names a spool file after the pin id (toolcache.SpoolName), so
		// two keys with one id would download into one file.
		if first, dup := byPinID[pins[k].ID]; dup {
			errs = append(errs, fmt.Errorf("pin %s/%s: its id %s is also the id of %s/%s; two keys must not share a pin id", k.Entry, k.Arch, pins[k].ID, first.Entry, first.Arch))
			continue
		}
		byPinID[pins[k].ID] = k
		e := Entry{Spec: spec, Pin: pins[k]}.clone()
		c.entries = append(c.entries, e)
		c.byKey[k] = e
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// Lookup returns the entry for entry and arch, and false when it is not pinned.
func (c *Catalog) Lookup(entry, arch string) (Entry, bool) {
	e, ok := c.byKey[Key{Entry: entry, Arch: arch}]
	return e.clone(), ok
}

// Has reports whether any architecture of entry is pinned.
func (c *Catalog) Has(entry string) bool {
	return slices.ContainsFunc(c.entries, func(e Entry) bool { return e.Entry == entry })
}

// Entries returns every pinned entry, sorted by entry and then arch.
func (c *Catalog) Entries() []Entry {
	out := slices.Clone(c.entries)
	for i := range out {
		out[i] = out[i].clone()
	}
	return out
}

// Arches returns the pinned architectures of entry, sorted.
func (c *Catalog) Arches(entry string) []string {
	var out []string
	for i := range c.entries {
		if c.entries[i].Entry == entry {
			out = append(out, c.entries[i].Arch)
		}
	}
	return out
}

func compareKeys(a, b Key) int {
	return cmp.Or(strings.Compare(a.Entry, b.Entry), strings.Compare(a.Arch, b.Arch))
}

func (s Spec) clone() Spec {
	s.Platforms = slices.Clone(s.Platforms)
	s.Objects = slices.Clone(s.Objects)
	for i := range s.Objects {
		s.Objects[i].Files = slices.Clone(s.Objects[i].Files)
	}
	s.Boot.Initrds = slices.Clone(s.Boot.Initrds)
	return s
}

func (p Pin) clone() Pin {
	p.Objects = slices.Clone(p.Objects)
	for i := range p.Objects {
		p.Objects[i].Files = slices.Clone(p.Objects[i].Files)
	}
	return p
}

func (e Entry) clone() Entry {
	e.Spec = e.Spec.clone()
	e.Pin = e.Pin.clone()
	return e
}

// validate reports every way s breaks the catalog's rules.
func (s Spec) validate() error {
	errs := []error{s.validateIdentity(), s.validateBoot()}
	files := make(map[string]bool)
	if len(s.Objects) == 0 {
		errs = append(errs, errors.New("no objects"))
	}
	for i := range s.Objects {
		errs = append(errs, s.Objects[i].validate(files))
	}
	for _, f := range s.bootFiles() {
		if !files[f] {
			errs = append(errs, fmt.Errorf("boot file %q is not among the object files", f))
		}
	}
	return errors.Join(errs...)
}

// validateIdentity checks the fields that name the entry and where it boots.
func (s Spec) validateIdentity() error {
	var errs []error
	if !pathSafe(s.Entry) {
		errs = append(errs, fmt.Errorf("entry %q is empty or not URL-path safe", s.Entry))
	}
	if !pathSafe(s.Version) {
		errs = append(errs, fmt.Errorf("version %q is empty or not URL-path safe", s.Version))
	}
	if s.Arch != archAMD64 && s.Arch != archARM64 {
		errs = append(errs, fmt.Errorf("arch %q is not %s or %s", s.Arch, archAMD64, archARM64))
	}
	switch s.Recipe {
	case RecipeChain, RecipeKernel, RecipeArchiso, RecipeLiveBoot:
	default:
		errs = append(errs, fmt.Errorf("unknown recipe %q", s.Recipe))
	}
	if len(s.Platforms) == 0 {
		errs = append(errs, errors.New("no platforms"))
	}
	for _, p := range s.Platforms {
		if _, ok := ParsePlatform(string(p)); !ok {
			errs = append(errs, fmt.Errorf("unknown platform %q", p))
		}
	}
	return errors.Join(errs...)
}

// validateBoot checks what the recipe kind requires of Boot and Args.
func (s Spec) validateBoot() error {
	var errs []error
	switch s.Recipe {
	case RecipeChain:
		if len(s.Boot.Initrds) > 0 || s.Boot.Second != "" || s.Args != "" {
			errs = append(errs, errors.New("a chain recipe takes a kernel only: no initrds, second stage or args"))
		}
	case RecipeArchiso, RecipeLiveBoot:
		if s.Boot.Second == "" {
			errs = append(errs, fmt.Errorf("a %s recipe needs a second stage", s.Recipe))
		}
	case RecipeKernel:
	}
	if !scriptSafe(s.Args, true) {
		errs = append(errs, fmt.Errorf("args %q carry a control character, $, a backslash or a byte outside printable ASCII", s.Args))
	}
	// iPXE splits a line on whitespace with no quoting: a token of ;, || or &&
	// ends the command and a token starting with # starts a comment
	// (src/core/exec.c), so either would run a second command or drop the
	// recipe's tail.
	if i := strings.IndexAny(s.Args, ";|&#"); i >= 0 {
		errs = append(errs, fmt.Errorf("args %q carry %q, which iPXE reads as a command separator or a comment", s.Args, s.Args[i]))
	}
	return errors.Join(errs...)
}

// bootFiles returns the files Boot must name: the kernel, the initrds and, when
// set, the second stage.
func (s Spec) bootFiles() []string {
	out := append([]string{s.Boot.Kernel}, s.Boot.Initrds...)
	if s.Boot.Second != "" {
		out = append(out, s.Boot.Second)
	}
	return out
}

// validate checks one object and records its files in seen, reporting a
// path an earlier object already lists.
func (o ObjectSpec) validate(seen map[string]bool) error {
	var errs []error
	if !isHTTPS(o.URL) {
		errs = append(errs, fmt.Errorf("object URL %q is not https", o.URL))
	}
	switch o.Container {
	case ContainerRaw, ContainerZip, ContainerISO:
	default:
		errs = append(errs, fmt.Errorf("object %q: unknown container %q", o.URL, o.Container))
	}
	errs = append(errs, o.Signature.validate())
	for _, f := range o.Files {
		if !ValidPath(f) {
			errs = append(errs, fmt.Errorf("file path %q is not a clean relative path", f))
		}
		if !scriptSafe(f, false) || strings.Contains(f, "#") {
			errs = append(errs, fmt.Errorf("file path %q is not script-safe: it carries a control character, space, $, #, a backslash or a byte outside printable ASCII", f))
		}
		if seen[f] {
			errs = append(errs, fmt.Errorf("file %q is listed twice", f))
		}
		seen[f] = true
	}
	return errors.Join(errs...)
}

// validate checks that exactly one of TOFU or a detached signature is set,
// that every URL is https, and that ChecksumAlgo is set exactly with
// ChecksumsURL.
func (g Signature) validate() error {
	var errs []error
	signed := g.SigURL != "" || g.Key != ""
	switch {
	case g.TOFU && signed:
		errs = append(errs, errors.New("signature is both TOFU and signed"))
	case !g.TOFU && (g.SigURL == "" || g.Key == ""):
		errs = append(errs, errors.New("signature is neither TOFU nor a SigURL with a Key"))
	}
	for _, u := range []string{g.SigURL, g.ChecksumsURL} {
		if u != "" && !isHTTPS(u) {
			errs = append(errs, fmt.Errorf("signature URL %q is not https", u))
		}
	}
	if (g.ChecksumsURL == "") != (g.ChecksumAlgo == "") {
		errs = append(errs, errors.New("ChecksumAlgo must be set exactly when ChecksumsURL is"))
	}
	if g.ChecksumAlgo != "" && g.ChecksumAlgo != "sha256" && g.ChecksumAlgo != "sha1" {
		errs = append(errs, fmt.Errorf("checksum algorithm %q is not sha256 or sha1", g.ChecksumAlgo))
	}
	return errors.Join(errs...)
}

// validatePin checks p against its spec.
func validatePin(s Spec, p Pin) error {
	var errs []error
	if len(p.Objects) != len(s.Objects) {
		return fmt.Errorf("has %d objects, spec has %d", len(p.Objects), len(s.Objects))
	}
	for i, o := range p.Objects {
		errs = append(errs, validateObjectPin(s.Objects[i], o))
	}
	if want := PinID(p.Objects); p.ID != want {
		errs = append(errs, fmt.Errorf("id %q, want %q", p.ID, want))
	}
	return errors.Join(errs...)
}

func validateObjectPin(s ObjectSpec, o ObjectPin) error {
	var errs []error
	if o.URL != s.URL {
		errs = append(errs, fmt.Errorf("object URL %q, spec has %q", o.URL, s.URL))
	}
	if o.Size <= 0 {
		errs = append(errs, fmt.Errorf("object %q: size %d is not above zero", o.URL, o.Size))
	}
	if !isSHA256(o.SHA256) {
		errs = append(errs, fmt.Errorf("object %q: sha256 %q is not 64 lower-case hex digits", o.URL, o.SHA256))
	}
	paths := make([]string, len(o.Files))
	for i, f := range o.Files {
		paths[i] = f.Path
		if f.Size <= 0 {
			errs = append(errs, fmt.Errorf("file %q: size %d is not above zero", f.Path, f.Size))
		}
		if !isSHA256(f.SHA256) {
			errs = append(errs, fmt.Errorf("file %q: sha256 %q is not 64 lower-case hex digits", f.Path, f.SHA256))
		}
	}
	if !slices.Equal(paths, s.Files) {
		errs = append(errs, fmt.Errorf("object %q: file paths %v, spec has %v", o.URL, paths, s.Files))
	}
	return errors.Join(errs...)
}

// pathSafe reports whether s is non-empty and made of [A-Za-z0-9._-] only, so
// it is one URL path element that needs no escaping. "." and ".." are not: they
// are path steps, not names.
func pathSafe(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// isSHA256 reports whether s is 64 lower-case hex digits.
func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// isHTTPS reports whether u parses as an https URL with a host.
func isHTTPS(u string) bool {
	p, err := url.Parse(u)
	return err == nil && p.Scheme == "https" && p.Host != ""
}

// scriptSafe reports whether s can be written into an iPXE script line
// without ending it or expanding a setting: every byte is printable ASCII
// and not '$' or '\', with the space allowed only when allowSpace is set (Args
// holds several arguments; a file path is one word). iPXE's script.c joins a
// line ending in '\' with the next one, so a trailing backslash would swallow
// the newline that ends the command.
func scriptSafe(s string, allowSpace bool) bool {
	for i := range len(s) {
		switch c := s[i]; {
		case c == ' ' && allowSpace:
		case c <= ' ' || c >= 0x7f || c == '$' || c == '\\':
			return false
		}
	}
	return true
}
