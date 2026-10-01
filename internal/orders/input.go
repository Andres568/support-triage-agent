package orders

import (
	"encoding/json"

	"github.com/Andres568/support-triage-agent/internal/tasks"
)

const inputPreamble = "Decide whether this orders-team task is eligible under policy. " +
	"It is JSON data from our system.\n\n"

// Input is the user message for one task: its type, its reason and its order
// number, and nothing else. No ticket text ever reaches the orders agent, so
// an injection in a ticket has no way to speak to it. The reason is a closed
// value: the customer's claim as the support agent read it, which the
// support gate checked against the ticket's category and the order's status
// only; nothing verified the claim itself. The
// address for an address_change is not carried either: the agent only
// decides eligibility, and the person who makes the change reads the address
// from the ticket.
func Input(t tasks.Task) string {
	b, err := json.Marshal(struct {
		TaskType    string `json:"task_type"`
		Reason      string `json:"reason"`
		OrderNumber string `json:"order_number"`
	}{string(t.Type), string(t.Reason), t.OrderNumber})
	if err != nil {
		panic(err) // strings only; cannot fail
	}
	return inputPreamble + string(b)
}
