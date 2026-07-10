# 02 — API Design: Consistent, Idempotent, Extensible

One convention for every service and BFF. A client (or agent) that has used one
endpoint can predict every other one.

## 1. Conventions

| Topic | Rule |
|---|---|
| Style | JSON over HTTP at BFFs; gRPC service-to-service (same resource model) |
| Naming | Plural nouns, kebab-case paths, snake_case fields: `POST /v1/orders`, `GET /v1/orders/{order_id}` |
| Versioning | Path major version `/v1`; additive changes only within a version; breaking ⇒ `/v2` with dual-running window |
| IDs | Prefixed ULIDs: `ord_01H…`, `usr_…`, `mer_…`, `drv_…`, `pay_…` — self-describing, sortable, unguessable |
| Time | RFC 3339 UTC only (`2026-07-10T02:15:00Z`); field names end `_at` |
| Money | Integer minor units + ISO currency: `{"amount": 12550, "currency": "THB"}`. Never floats |
| Pagination | Cursor-based: `?limit=20&cursor=…`; response has `next_cursor` (null at end). No offsets |
| Filtering | `?filter[status]=DELIVERED&filter[created_after]=…`; documented allow-list per endpoint |
| Partial reads | `?fields=order_id,status,eta` field masks on heavy resources |
| Writes | `POST` create, `PATCH` partial update (merge semantics), `PUT` never used, `DELETE` soft-delete |
| Concurrency | `ETag`/`If-Match` on mutable resources (menus, carts) → `412` on stale write |
| Status vocabulary | Enums are UPPER_SNAKE, closed sets published in the schema registry; clients must tolerate unknown values (forward compat) |
| Auth | `Authorization: Bearer <token>`; gateway introspects; services receive signed identity headers, never raw tokens |

## 2. Error envelope

Every non-2xx from every service/BFF, no exceptions:

```json
{
  "error": {
    "code": "ORDER_INVALID_TRANSITION",
    "message": "Order ord_01H… cannot move from DELIVERED to CANCELLED.",
    "details": [{"field": "status", "reason": "terminal_state"}],
    "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
    "retryable": false
  }
}
```

- `code` is a stable, machine-readable UPPER_SNAKE string from a central
  registry (`libs/errors`); `message` is human-only and may change.
- `trace_id` is the live trace (doc 04), so a user report → exact trace in one hop.
- HTTP mapping: `400` validation, `401/403` auth, `404` missing, `409` conflict
  /invalid transition, `422` domain rule, `429` rate limit, `5xx` server —
  `retryable` tells clients which are safe to retry.

## 3. Idempotency protocol

**Every mutating endpoint requires `Idempotency-Key`** (client-generated ULID/UUID).

Server algorithm (shared middleware in `libs/idempotency`, backed by Redis with
PG fallback):

1. `SETNX key {status: IN_FLIGHT, request_hash}` (TTL 24 h).
2. If new → run the handler; store `{status: DONE, response_code, response_body}`.
3. If existing and `DONE` with **same** `request_hash` → replay the stored
   response (`Idempotency-Replayed: true` header). Same key, **different** body
   → `409 IDEMPOTENCY_KEY_REUSED`.
4. If `IN_FLIGHT` → `409 IDEMPOTENCY_IN_PROGRESS` with `Retry-After` (protects
   against concurrent double-taps).

End-to-end effect-once: the handler's DB write + outbox row are one transaction
(doc 01 §3); consumers dedupe by `event_id` in their inbox. Result: a double
"Pay" tap, a BFF retry, and a Kafka redelivery all converge to **one** charge.

## 4. Contracts

### 4.1 Core order-flow endpoints (service level)

| Endpoint | Purpose | Notes |
|---|---|---|
| `POST /v1/quotes` | Price a cart (items, fees, surge, promos) | body: cart_id, delivery location, voucher; returns `quote_id` (10 min TTL) |
| `POST /v1/orders` | Checkout | body: `quote_id`, payment method; requires Idempotency-Key; returns order in `PAYMENT_PENDING` |
| `GET /v1/orders/{id}` | Order detail + current state | ETag; field masks |
| `POST /v1/orders/{id}:cancel` | Cancel (customer/merchant/ops) | verb-suffix `:action` pattern for non-CRUD ops; 409 on illegal state |
| `POST /v1/payments/{id}:capture` | Capture after delivery | saga-internal; idempotent |
| `POST /v1/dispatch/offers/{id}:accept` | Driver accepts an offer | first-accept-wins; losers get `409 OFFER_TAKEN` |
| `GET /v1/orders/{id}/tracking` | Live position + ETA | BFF may upgrade to SSE/WebSocket |

### 4.2 BFF endpoints (client-shaped)

BFFs expose the same conventions but aggregate: e.g.
`GET /v1/customer/home?lat=…&lng=…` → one payload with stores (search), fees
(pricing), ratings; `GET /v1/driver/offers/current` → offer + order summary +
route. BFF endpoints never invent semantics — they compose service calls.

### 4.3 Event contracts (async)

Envelope for every Kafka message:

```json
{
  "event_id": "evt_01H…",
  "event_type": "order.paid",
  "occurred_at": "2026-07-10T02:15:00Z",
  "trace_id": "4bf92f35…",
  "aggregate": {"type": "order", "id": "ord_01H…", "region": "bkk"},
  "schema_version": 3,
  "payload": { }
}
```

| Topic | Key | Producer → consumers |
|---|---|---|
| `order.created/paid/accepted/…` | `region:order_id` | order → payment, dispatch, notification, settlement |
| `payment.authorized/captured/failed/refunded` | `region:order_id` | payment → order, settlement, notification |
| `dispatch.offered/assigned/failed` | `region:order_id` | dispatch → order, driver-bff push, notification |
| `menu.updated`, `store.status_changed` | `merchant_id` | catalog → search, cart |
| `driver.location_updated` (sampled) | `region:driver_id` | tracking → dispatch, customer tracking |
| `rating.updated` | `merchant_id` | rating → search |

Compatibility: registry-enforced **backward** compatibility; add optional
fields only; consumers ignore unknown fields; `schema_version` bumps on shape
change within the same topic.

## 5. Extensibility — proving the structure holds

The test of "supports any complex feature": add these without breaking a rule.

| Feature | How it slots in |
|---|---|
| Scheduled orders | `POST /v1/orders` gains optional `scheduled_at`; state machine gains `SCHEDULED → PAYMENT_PENDING` (timer-driven); no endpoint or event changes shape |
| Group orders | New sub-resource `POST /v1/orders/{id}/participants`; cart items carry `participant_id`; quote splits by participant — additive fields only |
| Multi-vendor cart | Checkout fans out to N child orders sharing a `group_id`; each child runs the standard saga; BFF presents them as one — zero service API changes |
| Subscriptions / meal plans | New `subscription` service that *creates* standard orders on schedule — composition, not modification |
| Surge & dynamic fees | Entirely inside `pricing-promo.quote`; quote response already itemizes `fees[]` with typed line items |
| New client (e.g. kiosk) | New BFF only; services untouched |

Rules that make this work: additive-only within a version, typed line-item
lists instead of scalar fields (`fees[]`, `discounts[]`, `details[]`),
`:action` verbs for new operations, events carry the full aggregate snapshot
needed by consumers (no N+1 read-back).
