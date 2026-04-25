// Package contract validates jambonz responses against hand-authored JSON
// Schemas in schemas/ at the repo root. See ADR-0015 and schemas/README.md.
//
// The package has no knowledge of swagger / OpenAPI / @jambonz/schema. It
// loads a specific schema file for a specific operation+status and validates
// one response body at a time.
package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// ErrNoSchema is returned when no schema is registered for a given
// operation+status. Per ADR-0015 callers must treat this as a test failure —
// never a silent pass.
var ErrNoSchema = errors.New("contract: no schema file for this interaction")

// Validator is the entry point for response validation.
type Validator struct {
	root   string // absolute path of schemas/ directory
	mu     sync.Mutex
	cache  map[string]*jsonschema.Schema // key = relative schema path
	comp   *jsonschema.Compiler
}

// New creates a Validator rooted at schemasRoot (typically <repo>/schemas).
func New(schemasRoot string) (*Validator, error) {
	abs, err := filepath.Abs(schemasRoot)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("schemas root %q: %w", abs, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("schemas root %q is not a directory", abs)
	}
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	return &Validator{
		root:  abs,
		cache: map[string]*jsonschema.Schema{},
		comp:  c,
	}, nil
}

// ValidateResponse validates body against the schema at
// schemas/<relPath>. relPath must be forward-slash style relative to the
// schemas root (e.g. "rest/applications/createApplication.response.201.json").
func (v *Validator) ValidateResponse(relPath string, body []byte) error {
	schema, err := v.load(relPath)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return fmt.Errorf("contract: empty body but schema expects content (%s)", relPath)
	}
	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		return fmt.Errorf("contract: response is not valid JSON (%s): %w", relPath, err)
	}
	if err := schema.Validate(data); err != nil {
		return fmt.Errorf("contract: %s: %w", relPath, err)
	}
	return nil
}

// Load pre-compiles a schema and returns an error early — useful for probes
// in TestMain.
func (v *Validator) Load(relPath string) error {
	_, err := v.load(relPath)
	return err
}

func (v *Validator) load(relPath string) (*jsonschema.Schema, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if s, ok := v.cache[relPath]; ok {
		return s, nil
	}
	abs := filepath.Join(v.root, filepath.FromSlash(relPath))
	if _, err := os.Stat(abs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNoSchema, relPath)
		}
		return nil, fmt.Errorf("stat %s: %w", relPath, err)
	}
	// Compile — santhosh-tekuri resolves $ref relative to the file path
	// automatically when we pass an absolute filesystem URL.
	uri := "file://" + abs
	s, err := v.comp.Compile(uri)
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", relPath, err)
	}
	v.cache[relPath] = s
	return s, nil
}
