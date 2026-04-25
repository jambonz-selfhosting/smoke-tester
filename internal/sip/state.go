package sip

import "fmt"

// State is the high-level call state. Methods on *Call return
// ErrInvalidState when the current state doesn't permit the operation.
type State int

const (
	StateInit       State = iota // created, no SIP sent/received yet
	StateTrying                  // sent or received 100 Trying
	StateRinging                 // sent or received 180 / 183
	StateAnswered                // 200 OK exchanged, media active
	StateEnded                   // BYE / CANCEL / reject final, call terminated
)

func (s State) String() string {
	switch s {
	case StateInit:
		return "init"
	case StateTrying:
		return "trying"
	case StateRinging:
		return "ringing"
	case StateAnswered:
		return "answered"
	case StateEnded:
		return "ended"
	}
	return fmt.Sprintf("state(%d)", int(s))
}

// Direction of the call leg from the harness's perspective.
type Direction int

const (
	Inbound  Direction = iota // jambonz dialed us; we're the UAS
	Outbound                  // we dialed jambonz; we're the UAC
)

func (d Direction) String() string {
	if d == Inbound {
		return "inbound"
	}
	return "outbound"
}

// ErrInvalidState is returned when a Call method is called from a state that
// doesn't permit it (e.g. Answer() on an outbound call, or Hangup() twice).
type ErrInvalidState struct {
	Operation string
	Got       State
	Allowed   []State
}

func (e *ErrInvalidState) Error() string {
	return fmt.Sprintf("sip: %s invalid in state %s (allowed: %v)", e.Operation, e.Got, e.Allowed)
}

func invalidState(op string, got State, allowed ...State) error {
	return &ErrInvalidState{Operation: op, Got: got, Allowed: allowed}
}
