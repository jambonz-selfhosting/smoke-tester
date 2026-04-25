package provision

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Paginated is the envelope jambonz uses for RecentCalls and Alerts.
// Fields come back as strings or ints depending on the endpoint (swagger
// typed them as integers but live returns strings). IntField absorbs both.
type Paginated struct {
	Total IntField         `json:"total"`
	Batch IntField         `json:"batch,omitempty"`
	Page  IntField         `json:"page"`
	Data  []map[string]any `json:"data"`
}

// IntField accepts either a JSON number or a numeric string and exposes Int().
type IntField int

func (i *IntField) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*i = 0
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("IntField: %w", err)
		}
		*i = IntField(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*i = IntField(n)
	return nil
}

func (i IntField) Int() int { return int(i) }
