// Package studiotests calls Agent Studio agents end to end: for each realtime voice API vendor with a key in
// .env it adds the vendor's credential to the jambonz account the Studio workspace is connected to, builds an
// order-desk agent on it, plays the scenario in scenarios/studio-s2s through a Studio test session, and checks
// the call against Studio's call record. Everything it creates is deleted at the end.
//
// The calls cost vendor credit and take minutes, so every test file carries `//go:build studio`: `make test`
// and `make test-report` compile this package to nothing. Run it with `make test-studio`.
package studiotests
