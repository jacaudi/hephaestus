package render

import (
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

// FuncMap returns the template functions of a definition: sprig.HermeticTxtFuncMap() restricted to AllowedFuncs().
func FuncMap() template.FuncMap {
	hermetic := sprig.HermeticTxtFuncMap()
	allowed := AllowedFuncs()
	funcs := make(template.FuncMap, len(allowed))

	for _, name := range allowed {
		if fn, ok := hermetic[name]; ok {
			funcs[name] = fn
		}
	}

	return funcs
}

// templateRule parses the template text of the file name with funcs, as the render does, and returns the load rule it
// breaks: a call to a function outside the allow-list, or a read of PatchesDir. It returns "" for a template that
// follows the rules, and for one that fails to parse for any other reason: the render fails the tree then.
func templateRule(name, text string, funcs template.FuncMap) string {
	tmpl, err := template.New(path.Base(name)).Funcs(funcs).Option("missingkey=error").Parse(text)
	if err != nil {
		// The parser reports a call to an unknown function as: template: <name>:<line>: function "<func>" not defined.
		_, rest, _ := strings.Cut(err.Error(), `function "`)
		if fn, _, ok := strings.Cut(rest, `" not defined`); ok {
			return ruleTemplateFunc + fn
		}

		return ""
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
