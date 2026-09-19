# Attach wire-shape conformance fixtures

Canonical JSON shapes for what the attach surface puts on the wire —
both the SSE event-stream protocol (see
`core-tui/docs/sse-event-stream-protocol.md`) and, since 2026-09-19,
the plain HTTP endpoints' response bodies. Fixtures are consumed by:

- **`pkg/attach/capabilities_conformance_test.go`** (SSE events) and
  **`pkg/attach/rest_conformance_test.go`** (REST responses) — each
  pins the wire format against the runtime types so a struct-tag
  rename or field reorder fails visibly.
- Downstream consumers (mast-web, core-tui) MAY mirror these
  fixtures into their own harness to verify a producer implements
  the spec correctly. The version stamp on each fixture identifies
  the minimum protocol version it targets.

## Fixture layout

Each event fixture is a single JSON document with the shape a
producer would emit on the SSE wire (i.e., the `data:` block,
decoded). Event fixture naming:
`<event-type>-<variant>-v<protocol-version>.json`. REST fixtures
follow their own convention — see "REST response fixtures" below.

| File | Event | Since |
|---|---|---|
| `capabilities-v1.4.0.json` | `capabilities` | 1.4.0 |
| `status-update-with-capabilities-v1.4.0.json` | `status-update` merge frame carrying an embedded `capabilities` hot-update | 1.4.0 |

## REST response fixtures

`rest-*.json` pin the JSON *response* shapes of the plain HTTP
endpoints the same way the event fixtures pin SSE frames. They exist
because mast reported the absence of exactly this to core-agent as
their #536: mast-web's bundled mock invented snake_case names for the
sessions list (`session_id` / `app_name` / `user_id`), its client was
written against the mock, and the drift had nothing to fail against
(mast-web#41). core-agent closed that with fixtures of their own
(#549); this is mast's side of its own report.

They are versioned independently of the SSE protocol: the `-v<N>`
suffix is a REST-shape version starting at 1, bumped on any wire-shape
change, with the old fixture kept frozen.

| File | Endpoint | Since |
|---|---|---|
| `rest-sessions-list-v1.json` | `GET /sessions` (the `{"sessions": [...]}` envelope; one `active` + one `idle` row) | v1 |
| `rest-create-session-v1.json` | `POST /sessions` → 201 body | v1 |
| `rest-whoami-v1.json` | `GET /whoami` (asserted-proxy variant — populates both `omitempty` fields) | v1 |
| `rest-inject-v1.json` | `POST /sessions/{app}/{sid}/inject` → 200 body | v1 |
| `rest-status-v1.json` | `GET /sessions/{app}/{sid}/status` → 200 body, running-with-a-tool variant | v1 |
| `rest-session-acl-v1.json` | `GET /sessions/{app}/{sid}/acl`, and the body of a successful `PUT` | v1 |
| `rest-usage-v1.json` | `GET /sessions/{app}/{sid}/usage` → 200 body, warm-then-hit variant | v1 |

Pinned by `rest_conformance_test.go`. Where a handler assembles its
envelope inline rather than marshalling a struct — `GET /sessions` and
`POST .../inject` — the test drives the live handler, because a fixture
compared against a map the test itself wrote agrees with itself no
matter what the handler does.

One shape is pinned *byte-exactly* rather than by key set: `GET /sessions`
with nothing to show is `{"sessions":[]}`, never `null`, so a client can
iterate without a nil check. That is a property of one `make(..., 0, n)` in
`listSessions`, and swapping it for a `var` declaration passes every other
test in the file — the key set is unchanged when there are no rows to have
keys. `TestConformance_RESTSessionsListEmptyIsArrayNotNull` is the only
thing standing between that one-character edit and every consumer that
trusted the array.

Four more things a client author should read off these rather than guess.

**`omitempty` does nothing to a `time.Time`.** `last_touched_at` on a
sessions row and `next_wake_at` on the status body are tagged
`omitempty` and are on the wire regardless — as
`0001-01-01T00:00:00Z` when they do not apply. Read them for the zero
value; do not treat presence as meaning the field applies. Pinned by
`TestConformance_OmitemptyDoesNothingToATimestamp`, which is there
because the live-handler test cannot tell "always emitted" from "this
row happened to have a value".

**Timestamps are RFC 3339 with arbitrary sub-second precision and an
arbitrary zone offset.** Active sessions-list rows carry the daemon's
local offset (they come from `time.Unix(0, ns)`), persisted rows are
typically UTC. Parse them; don't pattern-match the layout the examples
happen to show.

**`viewers` and `contributors` are the opposite convention to
everything else here** — always present, `[]` rather than `null` when
empty, so a client can append without a nil check. An owner-only ACL is
what every session starts with, so walking an empty one is the common
path. The `PUT` *request* body is a different shape and deliberately
unpinned: it is a whole-document replace, so an omitted list clears
that list rather than leaving it alone. (core-agent's route is a
`PATCH` with the opposite rule. Do not carry a client between them.)

**On the usage body, a cache *write* is reported as uncached input**,
because it bills at a premium over fresh input rather than a discount,
and `cost_usd_uncached_reference` is a counterfactual — "what this
would have cost with no prompt cache at all" — not a floor. The
warming turn in the fixture therefore has a reference *below* its cost.
That is the state of a cache that has been paid for and not yet
reused, and `TestConformance_RESTUsageWarmingTurnReadsBelowItsCost`
asserts it so a later edit cannot "correct" the fixture into an
example of the misreading it exists to prevent. `overall.turns` and
`overall.cost_usd` cover the whole session including spend restored
from a previous process; the token buckets cover this process only.

### What is NOT pinned

The pinned set is the endpoints core-agent fixtured that mast also
has, plus `GET .../usage` — whose eight token fields were declared
from the day the route shipped and only started carrying values in
mast#356, which makes a client written against the old response unable
to tell an unmeasured zero from a measured one.

Everything else on the surface is unpinned, and is listed here so the
gap is countable rather than unknown:

- **Read projections:** `GET .../tools`, `.../agents`, `.../subagents`,
  `.../context`, `.../memory`, `.../skills`, `.../mcp`, `.../pricing`,
  `.../perms`, `.../guardrails`.
- **Writes:** `POST .../wake`, `.../interrupt`, `.../perms/respond`,
  `.../perms/allow`, `.../perms/deny`, `.../guardrails/reset`,
  `.../pricing/refresh`, `.../pricing/set`, `.../reload`,
  `.../slash/{compact,done,btw,subagent,replan}`, `DELETE /sessions/...`.
- **Peers:** `POST /peers`, `GET /peers`, `DELETE /peers/{id}`,
  `POST /peers/{id}/heartbeat`.
- `GET .../events` and `GET .../perms/stream` are SSE, covered by the
  event fixtures above rather than here.

Extending the set is cheap and the next person should: add the fixture,
add a `TestConformance_REST*` case, add a row to the table above.

## Adding a new fixture

1. Add the file under `pkg/attach/testdata/conformance/` following
   the naming convention above.
2. Add (or extend) a test case in
   `pkg/attach/capabilities_conformance_test.go` (SSE) or
   `pkg/attach/rest_conformance_test.go` (REST) that constructs
   the runtime type, marshals it, and diffs against the fixture
   using `canonicalizeJSON`.
3. Bump the fixture version stamp when the wire shape changes.
   Fixtures are frozen to their spec version — a v1.4.0 fixture
   must round-trip against a v1.4.0-speaking producer indefinitely,
   and a `rest-*-v1.json` against a v1-speaking one.
4. Prove the new fixture is load-bearing before you trust it: rename
   the struct tag it pins and watch the test fail. A fixture written
   from the same marshal call the test makes will pass whatever the
   shape is.

## Why this lives here (and not in `core-tui`)

The sibling issue (`core-tui#…`) is landing the cross-repo harness
that will host the shared spec-adjacent fixtures. Until then, these
files live in-tree so each producer has a place to pin the wire
format. When the shared harness lands, the SSE fixtures move to it.

The REST fixtures probably do not: mast's handler set and
core-agent's have diverged past the point where one file describes
both. mast's ACL route is a `PUT` where theirs is a `PATCH` and is
readable at `SessionRead` where theirs requires `SessionAdmin`, mast's
status body carries four fields where theirs carries seven, mast has
no session titles, no per-subagent events route and no stop-agent
route, and `/usage` — which both daemons serve — is fixtured here and
not there. The fixtures here are
built from mast's own runtime types for that reason — what was taken
from core-agent#549 is the practice, not the files. A client that
speaks one daemon's REST surface should not assume it speaks the
other's.
