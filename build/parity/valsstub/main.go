// Command valsstub stands in for the vals binary in the parity image. Topf pipes a file that holds a secret reference
// through `vals eval -f -`; the stub answers with config.SubstituteReferences over the values in the JSON object at the
// path in VALSSTUB_VALUES, so the Topf reference render and hephaestus resolve every reference the same way.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"go.jacaudi.dev/hephaestus/internal/third_party/topf/config"
)

const (
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	status, message := run(os.Args[1:], os.Stdin, os.Stdout, os.Getenv("VALSSTUB_VALUES"))
	if message != "" {
		if _, err := fmt.Fprintln(os.Stderr, "vals: "+message); err != nil {
			os.Exit(exitFailure)
		}
	}

	os.Exit(status)
}

// run answers one `vals eval -f -` call. It returns the exit status and the line for stderr, empty on success. A message
// never holds a value.
func run(args []string, stdin io.Reader, stdout io.Writer, valuesPath string) (status int, message string) {
	if !slices.Equal(args, []string{"eval", "-f", "-"}) {
		return exitUsage, "only `eval -f -` is supported"
	}

	values, err := readValues(valuesPath)
	if err != nil {
		return exitFailure, err.Error()
	}

	content, err := io.ReadAll(stdin)
	if err != nil {
		return exitFailure, "read stdin: " + err.Error()
	}

	resolved, err := config.SubstituteReferences(content, values)
	if missing, ok := errors.AsType[*config.MissingReferenceError](err); ok {
		return exitFailure, "unknown reference " + missing.Ref
	}

	if err != nil {
		return exitFailure, err.Error()
	}

	if _, err := stdout.Write(resolved); err != nil {
		return exitFailure, "write stdout: " + err.Error()
	}

	return 0, ""
}

// readValues reads the JSON object (reference to value) at path.
func readValues(path string) (map[string]string, error) {
	if path == "" {
		return nil, errors.New("VALSSTUB_VALUES is not set")
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the parity harness's own fixture
	if err != nil {
		return nil, fmt.Errorf("read values: %w", err)
	}

	var values map[string]string
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, errors.New("values are not a JSON object of strings")
	}

	return values, nil
}
