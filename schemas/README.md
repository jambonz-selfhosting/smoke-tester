# `schemas/` — hand-authored JSON Schemas (draft 2020-12)

This directory is the **canonical contract** `smoke-tester` enforces on every jambonz response. Schemas here are hand-authored and committed to this repo — deliberately **not** sourced live from `api-server/lib/swagger/swagger.yaml` or `@jambonz/schema`. See [ADR-0015](../docs/adr/0015-contract-testing.md) for why.

## Why hand-authored and vendored

- **Drift detection.** If the upstream swagger changes (intentionally or not), tests keep passing against our pinned expectation until we deliberately update a schema here. That is the release-gate signal we want.
- **Reflect live behaviour, not the ideal spec.** Examples: the swagger says `Webhook.method` enum is `["get","post"]` but live jambonz returns `"POST"`; optional columns come back as `null`. Our schemas encode what actually happens so tests pass when the feature works.
- **No code generation.** Schemas are data; code reads them. Simple.

## Layout

```
schemas/
├── rest/
│   ├── common/                    # shared shapes referenced via $ref
│   │   ├── application.json
│   │   ├── general_error.json
│   │   ├── successful_add.json
│   │   └── webhook.json
│   └── <resource>/                # one directory per REST resource
│       └── <operationId>.response.<status>.json
├── verbs/                         # (future — Tier 3+)
├── callbacks/                     # (future — Tier 3+)
└── README.md
```

## Naming convention

`schemas/rest/<resource>/<operationId>.response.<status>.json`

- `<resource>` — lowercase, matches the swagger path segment (`applications`, `accounts`, `phone_numbers`).
- `<operationId>` — the swagger `operationId`, or `<method>_<path-camel-case>` when the swagger declares none (e.g. `get_applications_by_sid` for `GET /Applications/{ApplicationSid}` which has no operationId).
- `.response.<status>.json` — HTTP status code this schema applies to.

## Authoring rules

- **Draft 2020-12.** Top of every file: `"$schema": "https://json-schema.org/draft/2020-12/schema"`.
- **`additionalProperties: true`** on every object unless we deliberately want to forbid unknown fields. We want to catch renames/removals/type changes, not additions ([ADR-0015](../docs/adr/0015-contract-testing.md)).
- **`required`** lists only the fields we *demand* jambonz return. Everything else is either `"type": ["string", "null"]` or wrapped in `anyOf` with `{"type": "null"}`.
- **`$ref` is relative.** E.g. `"$ref": "../common/webhook.json"`. The loader resolves against the file's own path.
- **Comments go in `description`.** JSON doesn't support comments; we put authoring notes in the `description` field of the relevant schema.

## Changing a schema

1. Open the file and make the change.
2. Run `make test-rest` and ensure tests still pass.
3. Commit with a message that names the drift: e.g. `schemas: allow Application.tag as array (was string)`.
4. If the change relaxes a constraint to match live behaviour, add a `// TODO: upstream` comment *inside the description* linking to the issue filed against `api-server`/`@jambonz/schema`.

## Syncing from upstream — deliberately NOT automated

There is no auto-sync from `api-server` or `@jambonz/schema`. If you want to review a diff before updating, open both files side-by-side. Avoiding automation is the point: every schema change in this repo is a conscious contract update.
