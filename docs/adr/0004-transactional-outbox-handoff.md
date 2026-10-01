# 0004 — Two agents joined by a transactional outbox of structured tasks

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

Some tickets need work by the orders team: a reprint, an address change, a
refund review. The support agent reads untrusted customer text; the orders
agent decides eligibility against policy. The handoff must not lose or
duplicate work when a ticket is retried or its lease is lost, and it must
not become a path for a prompt injection to reach the second agent.

## Decision

- **Outbox in the same fenced transaction.** `support.TicketStore.
  FinalizeTicket` calls `queue.Finalize` with `tasks.Enqueue` as its `inTx`:
  the ticket's decision and its tasks commit together, or not at all when
  the fence fails (`ErrLostLease`, ADR-0002). A task exists if and only if
  its ticket was finalized. The agents never call each other; the tasks
  table is also the orders agent's queue.
- **A closed, structured contract** (`internal/handoff`, no dependencies):
  `FollowUp{Type, Reason, OrderNumber}`.
  - `Type` from `TaskTypes` (`reprint_request`, `address_change`,
    `refund_review`), mirrored by a CHECK (migration 00006);
  - `Reason` from `Reasons` (`damaged`, `misprint`, `lost_in_transit`,
    `cancelled_before_production`, `customer_request`), CHECK in 00008;
  - `OrderNumber` matching `OrderNumberRE` (`^ORD-[0-9]{6}$`), also a CHECK.
- **The email is copied by SQL**: `Enqueue` inserts
  `SELECT ..., t.customer_email FROM tickets t WHERE t.id = $1`; the model
  never supplies it.
- **No customer text crosses.** The orders agent's input (`orders.Input`)
  is only `task_type`, `reason` and `order_number`.
- **Validated three times before a row exists:** `Decision.Validate` in the
  loop (known type and reason, order regex, no repeated type, empty when
  escalating); `triage.Gate` phase 2 (drops all follow-ups on escalate or an
  unverified sender, drops any whose order is not in the sender-verified
  facts, and any whose reason does not fit the category or order status,
  `reasonProblem`); and `ValidateFollowUps` again in `FinalizeTicket`.
- **Duplicates.** `UNIQUE (ticket_id, type)` with `ON CONFLICT DO NOTHING`:
  a retried finalize never duplicates a task. Across tickets,
  `orders.TaskStore.EligibleDuplicate` turns a second `eligible` for the
  same order into `needs_human`; reprint and refund count as one job
  (compensation), so an eligible reprint makes a refund of the same order a
  duplicate and vice versa.
- **Compensation needs a human reply.** A `reprint_request` or
  `refund_review` follow-up forces the ticket to at least
  `draft_for_review` (`triage.Gate`, and `Record.Check` at the write
  boundary).

## Consequences

- Positive: exactly-once handoff without a broker; an injection in a ticket
  can at most choose among closed values for an order the sender owns;
  each agent has its own queue, prompt and eval.
- Negative: a new task type is a code change plus a migration (the CHECKs).
  The reason is the customer's claim as the support agent read it: checked
  for fit, never verified. The address for an `address_change` does not
  travel; the person executing it reads the ticket. `EligibleDuplicate`
  can miss a duplicate decided concurrently in the same batch (a person
  executes every proposal, so it costs a second look, not a second
  shipment).

## Alternatives considered

- **The support agent calls the orders agent directly:** couples their
  availability and retries, and passes context (often customer text) along.
- **Publish after commit (Pub/Sub or a second write):** a crash between the
  two loses or duplicates the task; the outbox avoids the dual write.
- **Free-text handoff notes:** flexible, but exactly the channel an
  injection would use to reach the second agent.
