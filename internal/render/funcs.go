package render

import (
	"fmt"
	"math"
	"path"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"

	"github.com/Masterminds/sprig/v3"
)

// AllowedFuncs returns the names of the template functions an imported definition may call, sorted: the 173 functions
// of sprig v3.3.0's HermeticTxtFuncMap whose output depends on their arguments alone. Together with DeniedFuncs they
// name every function of that map, and a test fails when a sprig upgrade adds one neither names, so an upgrade fails
// closed.
func AllowedFuncs() []string {
	return []string{
		"abbrev", "abbrevboth", "add", "add1", "add1f", "addf",
		"adler32sum", "all", "any", "append", "atoi", "b32dec",
		"b32enc", "b64dec", "b64enc", "base", "biggest", "buildCustomCert",
		"camelcase", "cat", "ceil", "chunk", "clean", "coalesce",
		"compact", "concat", "contains", "decryptAES", "deepCopy", "deepEqual",
		"default", "derivePassword", "dict", "dig", "dir", "div",
		"divf", "duration", "empty", "ext", "fail", "first",
		"float64", "floor", "fromJson", "get", "has", "hasKey",
		"hasPrefix", "hasSuffix", "hello", "indent", "initial", "initials",
		"int", "int64", "isAbs", "join", "kebabcase", "kindIs",
		"kindOf", "last", "list", "lower", "max", "maxf",
		"merge", "mergeOverwrite", "min", "minf", "mod", "mul",
		"mulf", "mustAppend", "mustChunk", "mustCompact", "mustDeepCopy", "mustFirst",
		"mustFromJson", "mustHas", "mustInitial", "mustLast", "mustMerge", "mustMergeOverwrite",
		"mustPrepend", "mustPush", "mustRegexFind", "mustRegexFindAll", "mustRegexMatch", "mustRegexReplaceAll",
		"mustRegexReplaceAllLiteral", "mustRegexSplit", "mustRest", "mustReverse", "mustSlice", "mustToJson",
		"mustToPrettyJson", "mustToRawJson", "mustUniq", "mustWithout", "nindent", "nospace",
		"omit", "osBase", "osClean", "osDir", "osExt", "osIsAbs",
		"pick", "pluck", "plural", "prepend", "push", "quote",
		"regexFind", "regexFindAll", "regexMatch", "regexQuoteMeta", "regexReplaceAll", "regexReplaceAllLiteral",
		"regexSplit", "repeat", "replace", "rest", "reverse", "round",
		"semver", "semverCompare", "seq", "set", "sha1sum", "sha256sum",
		"sha512sum", "slice", "snakecase", "sortAlpha", "split", "splitList",
		"splitn", "squote", "sub", "subf", "substr", "swapcase",
		"ternary", "title", "toDecimal", "toJson", "toPrettyJson", "toRawJson",
		"toString", "toStrings", "trim", "trimAll", "trimPrefix", "trimSuffix",
		"trimall", "trunc", "tuple", "typeIs", "typeIsLike", "typeOf",
		"uniq", "unset", "until", "untilStep", "untitle", "upper",
		"urlJoin", "urlParse", "without", "wrap", "wrapWith",
	}
}

// DeniedFuncs returns the names of the functions of sprig v3.3.0's HermeticTxtFuncMap that no definition may call,
// sorted: those whose output depends on the clock, on randomness, on Go's map iteration order, or on the host's time
// zone. Such a function would render differently each time, set drift on every config and hold the host forever.
func DeniedFuncs() []string {
	return []string{
		"ago", "bcrypt", "durationRound", "encryptAES", "genCA", "genCAWithKey",
		"genPrivateKey", "genSelfSignedCert", "genSelfSignedCertWithKey", "genSignedCert", "genSignedCertWithKey", "htpasswd",
		"keys", "mustDateModify", "mustToDate", "must_date_modify", "randInt", "shuffle",
		"toDate", "unixEpoch", "values",
	}
}

// The bounds on what one call of the functions that build a sequence may build: a definition is untrusted text, and
// a count it names is not.
const (
	maxRepeatBytes = 1 << 20
	maxSequence    = 100_000
)

// FuncMap returns the template functions of a definition: sprig.HermeticTxtFuncMap() restricted to AllowedFuncs(),
// with repeat, until, untilStep and seq refusing a call that would build more than maxRepeatBytes or maxSequence
// items.
func FuncMap() template.FuncMap {
	hermetic := sprig.HermeticTxtFuncMap()
	allowed := AllowedFuncs()
	funcs := make(template.FuncMap, len(allowed))

	for _, name := range allowed {
		if fn, ok := hermetic[name]; ok {
			funcs[name] = fn
		}
	}

	boundSequences(funcs)

	return funcs
}

// boundSequences replaces the functions of funcs that build a string or list as long as an argument says.
func boundSequences(funcs template.FuncMap) {
	// The signatures are sprig's; TestFuncMap_BoundedFunctionsKeepSprigResults fails on an upgrade that changes one.
	repeat := funcs["repeat"].(func(int, string) string)        //nolint:forcetypeassert // pinned by the test named above
	until := funcs["until"].(func(int) []int)                   //nolint:forcetypeassert // pinned by the test named above
	untilStep := funcs["untilStep"].(func(int, int, int) []int) //nolint:forcetypeassert // pinned by the test named above
	seq := funcs["seq"].(func(...int) string)                   //nolint:forcetypeassert // pinned by the test named above

	funcs["repeat"] = func(count int, s string) (string, error) {
		if count > 0 && s != "" && count > maxRepeatBytes/len(s) {
			return "", fmt.Errorf("repeat: %d copies of %d bytes exceed the limit of %d bytes", count, len(s), maxRepeatBytes)
		}

		return repeat(count, s), nil
	}
	funcs["until"] = func(count int) ([]int, error) {
		if err := checkSpan("until", 0, float64(count), 1); err != nil {
			return nil, err
		}

		return until(count), nil
	}
	funcs["untilStep"] = func(start, stop, step int) ([]int, error) {
		if err := checkSpan("untilStep", float64(start), float64(stop), float64(step)); err != nil {
			return nil, err
		}

		return untilStep(start, stop, step), nil
	}
	funcs["seq"] = func(params ...int) (string, error) {
		from, to, step := 1.0, 0.0, 1.0

		switch len(params) {
		case 1:
			to = float64(params[0])
		case 2: //nolint:mnd // seq start end
			from, to = float64(params[0]), float64(params[1])
		case 3: //nolint:mnd // seq start step end
			from, step, to = float64(params[0]), float64(params[1]), float64(params[2])
		}

		if err := checkSpan("seq", from, to, step); err != nil {
			return "", err
		}

		return seq(params...), nil
	}
}

// checkSpan refuses a sequence from from to to in steps of step that has more than maxSequence items. A step of 0
// builds none.
func checkSpan(name string, from, to, step float64) error {
	if step != 0 && math.Abs(to-from)/math.Abs(step) > maxSequence {
		return fmt.Errorf("%s: a sequence from %.0f to %.0f in steps of %.0f exceeds the limit of %d items", name, from, to, step, maxSequence)
	}

	return nil
}

// templateRule parses the template text of the file name with funcs, as the render does, and returns the load rule it
// breaks: a call to a function outside the allow-list, or a read of PatchesDir. It returns "" for a template that
// follows the rules, and for one that fails to parse for any other reason: the render fails the tree then.
func templateRule(name, text string, funcs template.FuncMap) string {
	tmpl, err := template.New(path.Base(name)).Funcs(funcs).Option("missingkey=error").Parse(text)
	if err != nil {
		return undefinedFuncRule(err, path.Base(name))
	}

	for _, t := range tmpl.Templates() {
		if t.Tree != nil && readsPatchesDir(t.Root) {
			return ruleTemplatePatchesDir
		}
	}

	return ""
}

// patchesDirField is the field of Topf's PatchContext that a template must not read: the data a template executes
// against has no such field, and a definition that names one was written for Topf's absolute directory.
const patchesDirField = "PatchesDir"

// readsPatchesDir reports whether the parse tree below n names PatchesDir in a field, chain or variable node.
func readsPatchesDir(n parse.Node) bool {
	switch n := n.(type) {
	case *parse.ListNode:
		return n != nil && slices.ContainsFunc(n.Nodes, readsPatchesDir)
	case *parse.ActionNode:
		return readsPatchesDir(n.Pipe)
	case *parse.PipeNode:
		return n != nil && slices.ContainsFunc(n.Cmds, func(c *parse.CommandNode) bool { return readsPatchesDir(c) })
	case *parse.CommandNode:
		return slices.ContainsFunc(n.Args, readsPatchesDir)
	case *parse.IfNode:
		return readsPatchesDirBranch(&n.BranchNode)
	case *parse.RangeNode:
		return readsPatchesDirBranch(&n.BranchNode)
	case *parse.WithNode:
		return readsPatchesDirBranch(&n.BranchNode)
	case *parse.TemplateNode:
		return readsPatchesDir(n.Pipe)
	case *parse.FieldNode:
		return slices.Contains(n.Ident, patchesDirField)
	case *parse.ChainNode:
		return slices.Contains(n.Field, patchesDirField) || readsPatchesDir(n.Node)
	case *parse.VariableNode:
		return slices.Contains(n.Ident, patchesDirField)
	default:
		return false
	}
}

func readsPatchesDirBranch(b *parse.BranchNode) bool {
	return readsPatchesDir(b.Pipe) || readsPatchesDir(b.List) || readsPatchesDir(b.ElseList)
}

// undefinedFuncRule returns the rule for a parse error that reports a call to an unknown function, "" for any other
// error. The parser words it "template: <name>:<line>: function "<func>" not defined"; the template name, which is the
// file's name and can hold any text, is cut off first so it cannot be mistaken for the function.
func undefinedFuncRule(err error, name string) string {
	rest, ok := strings.CutPrefix(err.Error(), "template: "+name+":")
	if !ok {
		return ""
	}

	_, msg, ok := strings.Cut(rest, ": ") // the line number holds no ": "
	if !ok {
		return ""
	}

	msg, ok = strings.CutPrefix(msg, `function "`)
	if !ok {
		return ""
	}

	if fn, _, ok := strings.Cut(msg, `" not defined`); ok {
		return ruleTemplateFunc + fn
	}

	return ""
}
