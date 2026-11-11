package inbound

import "gchat-adapter/internal/ledger"

// ReplayResult describes the replay's current outcome in adapter-owned words,
// without exposing the stored message or gc's raw error text.
func ReplayResult(c *ledger.ChangeState) (status, reason string) {
	if c == nil {
		return "waiting", "delivery state unavailable; check gc gchat status"
	}
	switch c.Status {
	case "committed":
		switch c.Reason {
		case "delivered":
			return "landed", "gc delivered the message"
		case reasonLanded:
			return "landed", "gc confirmed the whole message reached the terminal; submission is unconfirmed"
		case "no_route":
			return "not_routed", "gc has no route for this message; the adapter will not retry it"
		}
	case "pending":
		return "waiting", "gc has the message; waiting for its receipt"
	case "unresolved":
		reason = "waiting for an operator: " + c.Reason
		if c.Fragment {
			reason += "; clear the fragment first from the agent's input box, then replay"
		}
		return "waiting", reason
	case "intent":
		if c.Fragment {
			return "waiting", "gc reported a fragment in the agent's input box; the adapter is still retrying"
		}
		if meaning := causeMeaning[c.Cause]; meaning != "" {
			return "waiting", meaning + "; the adapter will retry"
		}
		if c.Reason == reasonRetry {
			return "waiting", "gc temporarily unavailable; the adapter will retry"
		}
	}
	return "waiting", "waiting for the adapter's next delivery pass; check gc gchat status"
}
