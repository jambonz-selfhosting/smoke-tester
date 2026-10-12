package provision

import (
	"encoding/json"
	"fmt"
)

// Flag is a MySQL BOOLEAN column: jambonz returns it as 0/1, but a JSON
// boolean is accepted too.
type Flag bool

func (f *Flag) UnmarshalJSON(b []byte) error {
	switch string(b) {
	case "null", "0", "false":
		*f = false
		return nil
	case "1", "true":
		*f = true
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("Flag: %s is not 0/1/true/false", b)
	}
	*f = n != 0
	return nil
}
