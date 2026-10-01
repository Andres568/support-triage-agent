# 0005 — Orders agent sees exactly one order

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

The orders agent decides whether a task (ADR-0004) is eligible under
policy. It needs one order's details. A general order lookup would let a
confused or manipulated model read any customer's order, making the agent a
confused deputy the moment anything untrusted reached it.

## Decision

- **An argument-less tool.** `tools.ForTask(c, orderNumber, customerEmail,
  reportedAt)` returns `get_task_order`, whose schema is
  `{"type":"object","properties":{},"additionalProperties":false}` and whose
  `Call` ignores its arguments. Order number and email are bound from the
  task row, which our code wrote (number verified for that sender by the
  support gate; email copied by SQL). The other tool is `search_policy`.
- **Owner-scoped API, no new permission.** The tool uses the same commerce
  endpoint as support: `commerce.Store` filters `WHERE order_number = $1
  AND lower(customer_email) = lower($2)`, so another customer's order is a
  404, the same as a missing one. There is no unrestricted lookup anywhere.
- **No write tools.** The agent only proposes; its autonomy is fixed at
  `suggest` (`cmd/worker`), the only final status is `proposed`, and a
  person executes every proposal.
- **Deterministic checks around the model:**
  - `orders.Baseline`: the same decision as plain rules (reprint windows,
    lost-parcel business days, address change before shipping, refund
    limit), computed from an order our code fetches after the loop;
  - `orders.Gate` is monotonic: disagreement with the baseline, a currency
    other than USD, reprint items not on the order or above the ordered
    quantity, or a refund above the total or the limit make the final
    `needs_human`; it never makes a verdict more permissive;
  - `Outcome.Check` in `FinalizeTask`: an `eligible` final must be the
    agent's proposal unchanged, with an eligible baseline and no overrides;
    other verdicts propose no items and no refund.
- The model sees order items reduced to `sku`, `qty` and unit price
  (`tools.forModel`): product names are free text and could carry
  instructions.

## Consequences

- Positive: whatever a model is told, it can read one order, owned by the
  task's customer. `internal/tools/fortask_test.go` covers the binding. The
  baseline doubles as the retirement benchmark: if rules alone match the
  agent's final accuracy, the LLM adds cost without value (ADR-0009,
  `docs/autonomy.md`).
- Negative: the agent cannot look at related orders (a customer's earlier
  order, a split shipment); such tasks end as `needs_human`. `eligible`
  requires model and rules to agree, so the agent can never be more lenient
  than the rules.

## Alternatives considered

- **`get_order(order_number)` with the email bound**, as support has: still
  lets a model read any of that customer's orders, which the task does not
  need.
- **A privileged internal endpoint without owner scoping:** a new
  permission, and the confused-deputy risk this ADR exists to avoid.
- **Rules only, no model:** kept as the baseline and benchmark; the model
  stays only while it beats it.
