#!/usr/bin/env bash
# Verify evals/golden.jsonl against the seeded tickets:
#   - the file parses, and every row has the expected shape and types
#   - category and action use only known values
#   - follow_ups are well-formed (closed type and reason lists, order number format, no extra keys)
#   - must_not_contain, when present, is a non-empty list of non-empty strings
#   - every seeded ticket has exactly one golden label and vice versa
# and evals/golden_tasks.jsonl (orders agent labels):
#   - every row is {ticket, type, reason, order_number, verdict[, refund_cents]}, nothing else
#   - type, reason and verdict use only known values, order numbers are well-formed
#   - (ticket, type) is unique, the ticket is seeded and its sender owns the order
# Usage: scripts/check-golden.sh <database-url>
set -euo pipefail

db_url=${1:?usage: $0 <database-url>}
golden=evals/golden.jsonl

categories='["order_status","shipping_delay","damaged_or_misprint","refund_request","order_change","general"]'
actions='["auto_reply","draft_for_review","escalate"]'
# Must match handoff.TaskTypes / handoff.OrderNumberRE and the tasks CHECKs.
task_types='["reprint_request","address_change","refund_review"]'
# Must match handoff.Reasons and the CHECK on tasks.reason.
reasons='["damaged","misprint","lost_in_transit","cancelled_before_production","customer_request"]'
# \A and \z, not ^ and $: in jq's regex dialect $ also matches before a newline.
order_number_re='\AORD-[0-9]{6}\z'

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Slurp every row into one array first. jq then runs once over one input, so
# any parse or runtime error is its exit status and stops the script (set -e);
# per-line jq calls only report the status of the last line.
jq -n '[inputs]' "$golden" > "$tmp/rows.json"

# Same rules as triage.Decision.Validate: follow_ups is required, types come
# from a closed list, order numbers are well-formed, escalations carry none.
# Type guards first, so a malformed row is reported instead of crashing jq.
problems=$(jq -r --argjson c "$categories" --argjson a "$actions" \
  --argjson t "$task_types" --argjson rs "$reasons" --arg re "$order_number_re" '
  .[] | . as $r
  | if type != "object" then "row is not an object: \(tojson)"
    else
      ($r.external_id | if type == "string" then . else tojson end) as $id
      | (if ($r.external_id | type) != "string" then "\($id): external_id must be a string" else empty end),
        (if ($r.category | IN($c[]) | not) then "\($id): unknown category \($r.category | tojson)" else empty end),
        (if ($r.action | IN($a[]) | not) then "\($id): unknown action \($r.action | tojson)" else empty end),
        (if ($r | has("must_not_contain")) and (($r.must_not_contain | type) != "array"
              or ($r.must_not_contain | length) == 0
              or ($r.must_not_contain | any(type != "string" or . == "")))
         then "\($id): must_not_contain must be a non-empty array of non-empty strings" else empty end),
        (if ($r.follow_ups | type) != "array" then "\($id): follow_ups must be an array"
         else
           ($r.follow_ups[]
            | if type != "object" then "\($id): follow-up is not an object: \(tojson)"
              else
                ((keys - ["type", "reason", "order_number"]) as $extra
                 | if $extra != [] then "\($id): unknown follow-up keys \($extra | tojson)" else empty end),
                (if (.type | IN($t[]) | not) then "\($id): unknown follow-up type \(.type | tojson)" else empty end),
                (if (.reason | IN($rs[]) | not) then "\($id): unknown follow-up reason \(.reason | tojson)" else empty end),
                (if (.order_number | type) != "string" then "\($id): order_number must be a string"
                 elif (.order_number | test($re) | not) then "\($id): bad order_number \(.order_number | tojson)"
                 else empty end)
              end),
           ([$r.follow_ups[] | objects | .type] as $types
            | if ($types | length) != ($types | unique | length) then "\($id): repeated follow-up type" else empty end),
           (if $r.action == "escalate" and ($r.follow_ups | length) > 0 then "\($id): escalate must have no follow_ups" else empty end)
         end)
    end' "$tmp/rows.json")
if [[ -n $problems ]]; then
  echo "invalid golden.jsonl:" >&2
  echo "$problems" >&2
  exit 1
fi

jq -r '.[].external_id' "$tmp/rows.json" | sort > "$tmp/golden"
psql "$db_url" -tA -c 'SELECT external_id FROM tickets ORDER BY 1' | sort > "$tmp/db"

if ! diff -u "$tmp/golden" "$tmp/db"; then
  echo "golden.jsonl and seeded tickets differ (- golden, + db)" >&2
  exit 1
fi

dupes=$(uniq -d "$tmp/golden")
if [[ -n $dupes ]]; then
  echo "duplicate golden labels: $dupes" >&2
  exit 1
fi

echo "golden OK: $(wc -l < "$tmp/golden" | tr -d ' ') labels match seeded tickets"

# --- golden_tasks.jsonl -------------------------------------------------------
golden_tasks=evals/golden_tasks.jsonl
# Must match orders.Verdicts.
verdicts='["eligible","not_eligible","needs_human"]'

jq -n '[inputs]' "$golden_tasks" > "$tmp/tasks.json"

problems=$(jq -r --argjson t "$task_types" --argjson rs "$reasons" --argjson v "$verdicts" --arg re "$order_number_re" '
  (.[] | . as $r
   | if type != "object" then "row is not an object: \(tojson)"
     else
       ($r.ticket | if type == "string" then . else tojson end) as $id
       | ((keys - ["ticket", "type", "reason", "order_number", "verdict", "refund_cents"]) as $extra
          | if $extra != [] then "\($id): unknown keys \($extra | tojson)" else empty end),
         (if ($r.ticket | type) != "string" then "\($id): ticket must be a string" else empty end),
         (if ($r.type | IN($t[]) | not) then "\($id): unknown type \($r.type | tojson)" else empty end),
         (if ($r.reason | IN($rs[]) | not) then "\($id): unknown reason \($r.reason | tojson)" else empty end),
         (if ($r.verdict | IN($v[]) | not) then "\($id): unknown verdict \($r.verdict | tojson)" else empty end),
         (if ($r.order_number | type) != "string" then "\($id): order_number must be a string"
          elif ($r.order_number | test($re) | not) then "\($id): bad order_number \($r.order_number | tojson)"
          else empty end)
     end),
  ([.[] | objects | [.ticket, .type]] | group_by(.) | map(select(length > 1) | .[0])
   | .[] | "\(.[0]): repeated task type \(.[1])")' "$tmp/tasks.json")
if [[ -n $problems ]]; then
  echo "invalid golden_tasks.jsonl:" >&2
  echo "$problems" >&2
  exit 1
fi

# Tasks are inserted by the eval harness with the ticket's sender as the
# customer, so the sender must own the order (or the lookup would 404).
mismatched=$(psql "$db_url" -tA -v ON_ERROR_STOP=1 -v rows="$(cat "$tmp/tasks.json")" <<'SQL'
SELECT r->>'ticket'
FROM jsonb_array_elements(:'rows'::jsonb) r
LEFT JOIN tickets t ON t.external_id = r->>'ticket'
LEFT JOIN orders o ON o.order_number = r->>'order_number'
WHERE t.id IS NULL OR o.customer_email IS DISTINCT FROM t.customer_email;
SQL
)
if [[ -n $mismatched ]]; then
  echo "golden_tasks.jsonl rows whose ticket is not seeded or whose sender does not own the order:" >&2
  echo "$mismatched" >&2
  exit 1
fi

echo "golden tasks OK: $(jq length "$tmp/tasks.json") labels"
