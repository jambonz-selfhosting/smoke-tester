// Package failover checks how sbc-inbound copes with a feature server that
// dies: new calls must move on to a live feature server, and calls already on
// the dead one must be hung up instead of left in dead air.
//
// The tests break the cluster on purpose (a fake member in the active-fs set,
// a frozen feature-server process), so they run over ssh against a test box
// and are gated behind the `failover` build tag. Run with `make test-failover`.
//
// The dead-feature-server tests need the media watch turned on in sbc-inbound
// on the box (JAMBONES_FS_MEDIA_TIMEOUT_MS, e.g. 15000); it is off by default.
//
// Extra .env settings:
//
//	FAILOVER_SSH       ssh target of the all-in-one box, e.g. jambonz@hoan.jambonz.io
//	FAILOVER_SSH_KEY   private key for that target
//	FAILOVER_DEAD_FS   ip:port of a feature server that does not exist, on a
//	                   different host from the real one, e.g. 10.0.45.99:5070
package failover
