package contract

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ResolveSchemasRoot walks up from cwd looking for the repo's schemas/
// directory. Override via JAMBONZ_IT_SCHEMAS.
func ResolveSchemasRoot() (string, error) {
	if p := os.Getenv("JAMBONZ_IT_SCHEMAS"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("JAMBONZ_IT_SCHEMAS=%q: %w", p, err)
		}
		return p, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, "schemas")
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("schemas/ directory not found walking up from cwd — set JAMBONZ_IT_SCHEMAS")
		}
		dir = parent
	}
}
