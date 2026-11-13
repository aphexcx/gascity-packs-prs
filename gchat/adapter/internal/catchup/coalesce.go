package catchup

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
)

// prepareMerged records a bounded slice of the pass before settling its backlog
// rows. Budget and delivered counts remain source acts, not POSTs. Pending rows
// follow their delivery owner and keep the interval's cursors where they were.
func (r *Runner) prepareMerged(ctx context.Context, name string, budget *int) (int, error) {
	delivered := 0
	for *budget > 0 {
		before, err := r.snapshot()
		if err != nil {
			return delivered, err
		}
		s, err := spaceState(before, name)
		if err != nil {
			return delivered, err
		}
		p, err := decodeProgress(s.PageToken)
		if err != nil {
			return delivered, err
		}
		if p.unavailableHeld() || p.Phase != "done" {
			return delivered, nil
		}
		var indexes []int
		var changes []puller.Change
		for i, raw := range s.Backlog {
			var item queuedChange
			if json.Unmarshal([]byte(raw), &item) != nil {
				return delivered, errors.New("catchup: invalid durable backlog")
			}
			if item.Handled {
				continue
			}
			if retained := before.Changes[unavailableID(item)]; retained != nil && retained.Status == "committed" {
				continue
			}
			if item.Malformed != "" || inbound.ValidateChange(item.Change) != nil {
				if err := r.quarantine(name, item); err != nil {
					return delivered, err
				}
				indexes = append(indexes, i)
			} else {
				item.Change.At = item.At
				indexes = append(indexes, i)
				changes = append(changes, item.Change)
			}
			if len(indexes) >= *budget {
				break
			}
		}
		if len(indexes) == 0 {
			break
		}
		if len(changes) > 0 {
			out, err := r.opts.Processor.HandleChanges(ctx, changes)
			if err != nil {
				return delivered, err
			}
			if out != puller.Ack {
				return delivered, errors.New("catchup: batch was not acknowledged")
			}
		}
		after, err := r.snapshot()
		if err != nil {
			return delivered, err
		}
		for id, c := range after.Changes {
			if before.Changes[id] != nil {
				continue
			}
			if !strings.HasPrefix(c.OriginalKind(), "message.") && !strings.HasPrefix(c.OriginalKind(), "reaction.") {
				continue
			}
			switch c.Reason {
			case "own", "local", "wake_policy", "before_history_floor":
				continue
			}
			if strings.HasPrefix(c.Reason, "space unavailable") || strings.HasPrefix(c.Reason, "parent unavailable") {
				continue
			}
			*budget--
			if owner := after.Delivery(c); owner != nil && owner.Status == "committed" && owner.Reason == "delivered" {
				delivered++
			}
		}
		if err = r.update(func(l *ledger.Ledger) error {
			s, err := spaceState(l, name)
			if err != nil {
				return err
			}
			for _, i := range indexes {
				var item queuedChange
				if json.Unmarshal([]byte(s.Backlog[i]), &item) != nil {
					return errors.New("catchup: invalid durable backlog")
				}
				item.Handled = true
				s.Backlog[i] = encode(item)
			}
			return nil
		}); err != nil {
			return delivered, err
		}
	}
	return delivered, nil
}
