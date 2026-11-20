package render

import (
	"bytes"
	"errors"
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/Masterminds/sprig/v3"

	"go.jacaudi.dev/hephaestus/internal/third_party/topf/config"
)

func TestFuncMap_PartitionsHermeticMap(t *testing.T) {
	hermetic := sprig.HermeticTxtFuncMap()
	allowed, denied := AllowedFuncs(), DeniedFuncs()

	if !slices.IsSorted(allowed) || !slices.IsSorted(denied) {
		t.Error("AllowedFuncs and DeniedFuncs must each be sorted")
	}

	if len(allowed) != 173 || len(denied) != 21 {
		t.Errorf("len(AllowedFuncs), len(DeniedFuncs) = %d, %d, want 173, 21", len(allowed), len(denied))
	}

	for name := range hermetic {
		inAllowed, inDenied := slices.Contains(allowed, name), slices.Contains(denied, name)
		if inAllowed == inDenied {
			t.Errorf("sprig function %q: allowed = %t, denied = %t, want exactly one", name, inAllowed, inDenied)
		}
	}

	for _, name := range slices.Concat(allowed, denied) {
		if _, ok := hermetic[name]; !ok {
			t.Errorf("listed function %q is not in sprig.HermeticTxtFuncMap()", name)
		}
	}

	if len(allowed)+len(denied) != len(hermetic) {
		t.Errorf("allowed + denied = %d names, want the %d of the hermetic map", len(allowed)+len(denied), len(hermetic))
	}

	if got := slices.Sorted(maps.Keys(FuncMap())); !slices.Equal(got, allowed) {
		t.Errorf("FuncMap() keys differ from AllowedFuncs(): got %d names, want %d", len(got), len(allowed))
	}
}

func TestFuncMap_ListsAreFresh(t *testing.T) {
	first := AllowedFuncs()
	first[0] = "changed"

	if AllowedFuncs()[0] == "changed" {
		t.Error("AllowedFuncs() returned a shared slice")
	}

	denied := DeniedFuncs()
	denied[0] = "changed"

	if DeniedFuncs()[0] == "changed" {
		t.Error("DeniedFuncs() returned a shared slice")
	}
}

func TestFuncMap_DeniedAbsent(t *testing.T) {
	funcs := FuncMap()

	for _, name := range DeniedFuncs() {
		if _, ok := funcs[name]; ok {
			t.Errorf("FuncMap() holds denied function %q", name)
		}
	}

	fsys := baseTree(t)
	apply(t, fsys, writeFile("all/keys.yaml.tpl", "x: {{ keys .Data }}\n"))

	_, err := Load(fsys)

	refusal, ok := errors.AsType[*RefusalError](err)
	if !ok {
		t.Fatalf("Load error = %v, want a *RefusalError", err)
	}

	if want := (RefusalError{File: "all/keys.yaml.tpl", Rule: "template function not allowed: keys"}); *refusal != want {
		t.Errorf("Load refusal = %+v, want %+v", *refusal, want)
	}
}

// repeatableTemplate calls one function of each family: strings, lists, dicts, math, encodings, regex, semver, paths,
// hashes, derivePassword, decryptAES and toJson of a map.
const repeatableTemplate = `{{ upper "abc" }}|{{ title "hello world" }}|{{ trunc 3 "abcdef" }}|{{ replace "a" "b" "banana" }}
{{ list 3 1 2 | sortAlpha | join "," }}|{{ list 1 2 3 | reverse | toJson }}|{{ uniq (list 1 1 2) | toJson }}
{{ dict "b" 2 "a" 1 "c" (dict "z" 1 "y" 2) | toJson }}|{{ merge (dict "a" 1) (dict "b" 2) | toJson }}|{{ pick (dict "a" 1 "b" 2) "a" | toJson }}
{{ add 1 2 3 }}|{{ mul 2 3 }}|{{ max 1 5 3 }}|{{ divf 7 2 }}|{{ seq 1 3 }}
{{ b64enc "hello" }}|{{ b64dec "aGVsbG8=" }}|{{ b32enc "hello" }}
{{ regexReplaceAll "a+" "baaab" "-" }}|{{ regexFindAll "[0-9]+" "a1b22" -1 | toJson }}
{{ (semver "1.2.3").Major }}|{{ semverCompare "^1.0.0" "1.2.3" }}
{{ osBase "/a/b.txt" }}|{{ osDir "/a/b.txt" }}|{{ base "a/b.txt" }}
{{ sha256sum "hello" }}|{{ sha1sum "hello" }}|{{ adler32sum "hello" }}
{{ derivePassword 1 "long" "password" "user" "site" }}
{{ decryptAES "key" .cipher }}
{{ .map | toJson }}|{{ .map | toPrettyJson }}
`

func TestFuncMap_RepeatableOutput(t *testing.T) {
	encrypt, ok := sprig.HermeticTxtFuncMap()["encryptAES"].(func(string, string) (string, error))
	if !ok {
		t.Fatal("sprig's encryptAES has an unexpected signature")
	}

	cipher, err := encrypt("key", "plain text")
	if err != nil {
		t.Fatalf("encryptAES: %v", err)
	}

	data := map[string]any{
		"cipher": cipher,
		"map":    map[string]any{"k3": 3, "k1": 1, "k2": map[string]any{"b": 2, "a": 1}},
	}

	render := func() string {
		t.Helper()

		tmpl, err := template.New("t").Funcs(FuncMap()).Option("missingkey=error").Parse(repeatableTemplate)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		var buf bytes.Buffer

		if err := tmpl.Execute(&buf, data); err != nil {
			t.Fatalf("execute: %v", err)
		}

		return buf.String()
	}

	first := render()

	for _, want := range []string{"ABC", "1,2,3", "plain text", "aGVsbG8=", "1 2 3", "1|true"} {
		if !strings.Contains(first, want) {
			t.Errorf("rendered output lacks %q:\n%s", want, first)
		}
	}

	if second := render(); second != first {
		t.Errorf("second render differs from the first:\n%s\n--- first ---\n%s", second, first)
	}
}

// templateMethodAllowList is the reviewed set of methods a template can reach from config.TemplateData, as
// "<type>.<method>". All are pure: no I/O, no clock, no randomness.
func templateMethodAllowList() map[string]bool {
	allowed := map[string]bool{"config.TemplateNode.Endpoint": true}

	// The value methods of netip.Addr, and through *netip.Addr its two unmarshalers, which change only the render's own copy.
	for name := range strings.FieldsSeq(`AppendBinary AppendText AppendTo As16 As4 AsSlice BitLen Compare Is4 Is4In6 Is6
		IsGlobalUnicast IsInterfaceLocalMulticast IsLinkLocalMulticast IsLinkLocalUnicast IsLoopback IsMulticast IsPrivate
		IsUnspecified IsValid Less MarshalBinary MarshalText Next Prefix Prev String StringExpanded Unmap WithZone Zone
		UnmarshalBinary UnmarshalText`) {
		allowed["netip.Addr."+name] = true
	}

	return allowed
}

// reachableMethods returns "<type>.<method>" for every exported method of every named type a template can reach from
// root through exported fields, pointers, slices, arrays and maps (the method set of the pointer type, so pointer
// receivers count).
func reachableMethods(root reflect.Type) []string {
	var (
		methods []string
		seen    = map[reflect.Type]bool{}
		visit   func(reflect.Type)
	)

	visit = func(typ reflect.Type) {
		if seen[typ] {
			return
		}

		seen[typ] = true

		kind := typ.Kind()

		if kind != reflect.Interface && kind != reflect.Pointer {
			for method := range reflect.PointerTo(typ).Methods() {
				methods = append(methods, typ.String()+"."+method.Name)
			}
		}

		if kind == reflect.Pointer || kind == reflect.Slice || kind == reflect.Array || kind == reflect.Map {
			visit(typ.Elem())
		}

		if kind == reflect.Map {
			visit(typ.Key())
		}

		if kind == reflect.Struct {
			for field := range typ.Fields() {
				if field.IsExported() {
					visit(field.Type)
				}
			}
		}
	}

	visit(root)

	return methods
}

func TestTemplateData_PlainOnly(t *testing.T) {
	allowed := templateMethodAllowList()
	reached := reachableMethods(reflect.TypeFor[config.TemplateData]())

	for _, name := range reached {
		if !allowed[name] {
			t.Errorf("template data reaches method %s, which is not on the reviewed allow-list", name)
		}
	}

	// The walk must reach the types the allow-list is about, or it checks nothing.
	for _, name := range []string{"config.TemplateNode.Endpoint", "netip.Addr.String", "netip.Addr.UnmarshalText"} {
		if !slices.Contains(reached, name) {
			t.Errorf("the walk did not reach %s", name)
		}
	}

	if got, want := len(allowed), 34; got != want {
		t.Errorf("len(allow-list) = %d, want %d", got, want)
	}

	// A guard on the guard: the netip.Addr value methods named above are exactly the ones this Go version has.
	valueMethods := reflect.TypeFor[netip.Addr]().NumMethod()
	if valueMethods != 31 {
		t.Errorf("netip.Addr has %d value methods, want the 31 the allow-list names", valueMethods)
	}
}
