# 0002 — Postgres work queue with leases and a (run, attempt) fencing token

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

Tickets (support agent) and tasks (orders agent) must each be finalized at
most once, even with several workers in one run, a worker that crashes
mid-item, and a scheduler that may fire twice (Cloud Scheduler does not skip
overlapping fires, ADR-0010). Volume is demo scale (tens to thousands of
rows) and Postgres is already the system of record.

## Decision

One generic queue, `internal/queue` (`Queue[T]`), used by both tables
(`support.TicketStore`, `orders.TaskStore`). Each row is a small state
machine: `pending → claimed → <final> | failed`.

- **Claim** (`Queue.Claim`): `SELECT ... FOR UPDATE SKIP LOCKED` over rows
  that are `pending`, or `claimed` with an expired `lease_until` and
  `attempts < max`; the same statement sets `status='claimed'`,
  `claimed_by=<run id>`, `lease_until=now()+lease` and `attempts+1`. The
  worker (`internal/worker.Run`) claims one row at a time, so a lease starts
  when its work starts.
- **Fencing token = (claimed_by, attempts).** `Queue.Finalize` updates
  `WHERE id=$1 AND status='claimed' AND claimed_by=$2 AND attempts=$3`, in
  one transaction with the caller's `inTx` (the tasks outbox, ADR-0004). Zero
  rows affected returns `ErrLostLease` and rolls back everything, including
  `inTx`. Because attempts rise on every claim, two goroutines of the *same*
  run cannot finalize each other's lease. There is deliberately no
  `lease_until > now()` check: an expired lease nobody reclaimed can still
  finish, since a reclaim would have changed the token.
- **Retry by lease expiry.** `Queue.Abandon` records `last_error` and leaves
  the row claimed: the backoff is the rest of the lease, so an error and a
  crash recover the same way. With `attempts >= MAX_ATTEMPTS` it sets
  `failed` directly.
- **`FailExhausted`** runs at the start of every batch and fails expired
  leases with no attempts left, which Claim would otherwise never pick up.
- **Lease invariant.** `worker.Config.Validate` requires
  `LEASE >= ITEM_TIMEOUT + 2 x FINALIZE_TIMEOUT + 15s` (defaults: 240s vs
  180s + 2 x 10s + 15s).
- Table and result column are spliced into SQL only from a fixed map
  (`resultColumns`: tickets→decision, tasks→proposal); `queue.New` panics on
  anything else.

## Consequences

- Positive: one mechanism for both agents; a retried or double-fired batch
  never finalizes an item twice or duplicates its tasks; the behavior is
  tested with concurrent goroutines against a real database
  (`internal/queue/queue_test.go`).
- Negative: pickup latency is the scheduler interval (polling, no push). A
  retry waits for the rest of the lease. The lease must outlast the slowest
  item (enforced at startup); a too-long lease slows crash recovery.
- Phase transition: the claim is one query with an OR over two partial
  indexes (`*_pending_idx`, `*_claimed_lease_idx`). That is fine at
  thousands of rows; at roughly 10^5 pending rows (a design estimate, not
  measured) split it into two claims, pending first, then expired leases.

## Alternatives considered

- **Pub/Sub:** at-least-once delivery still needs the same database
  idempotency and fencing, plus another moving part and emulator.
- **Temporal:** durable workflows are overkill for a one-step item; a
  cluster to run and learn for no correctness gain here.
- **Postgres advisory locks:** released when the connection drops, and
  invisible in the row, so neither an audit nor a reclaim can see who owns
  an item.
