# Architecture

Where the harness's shape and its interaction with jambonz is documented.
Complements the higher-level [ARCHITECTURE.md](../../ARCHITECTURE.md) at
repo root (system-level prose) with diagrammed, wire-level specifics.

## Index

- **[components.md](components.md)** — component diagram + per-component
  stack choice + traffic/protocol table. Read this when you need to know
  what talks to what and over which protocol.

(Append new architecture pages here as the harness grows — e.g.
`observability.md` once we wire metrics, `async-api.md` when the jambonz
WebSocket API is plumbed in.)
