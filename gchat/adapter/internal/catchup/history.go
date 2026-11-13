package catchup

import (
	"encoding/json"
	"fmt"
	"time"

	"gchat-adapter/internal/config"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
)

// InitializeHistory upgrades the saved inventory before any workers or polls
// can read it. It never gives an existing installation a global floor.
func InitializeHistory(l *ledger.Ledger, cfg *config.Config, now time.Time, log puller.Logger) error {
	_, err := InitializeHistoryWithReport(l, cfg, now, log)
	return err
}

// InitializeHistoryWithReport also returns each delivery's message reference
// for the caller to report only after saving the migrated ledger successfully.
func InitializeHistoryWithReport(l *ledger.Ledger, cfg *config.Config, now time.Time, log puller.Logger) ([]string, error) {
	if l.SpacesListedAt.IsZero() && l.HistoryFloor.IsZero() {
		l.SpacesListedAt = now.UTC()
	}
	for _, c := range l.Changes {
		if c.Reason == "before_history_floor" {
			c.Reason, c.Cause = "local", "before_history_floor"
		}
	}
	var retired []string
	for name, s := range l.Spaces {
		references, err := startSpaceWithReport(l, cfg, name, s, now, log)
		if err != nil {
			return nil, err
		}
		retired = append(retired, references...)
	}
	return retired, nil
}

func startSpace(l *ledger.Ledger, cfg *config.Config, name string, s *ledger.SpaceState, now time.Time, log puller.Logger) error {
	_, err := startSpaceWithReport(l, cfg, name, s, now, log)
	return err
}

func startSpaceWithReport(l *ledger.Ledger, cfg *config.Config, name string, s *ledger.SpaceState, now time.Time, log puller.Logger) ([]string, error) {
	if !l.StartSpace(s, now) {
		return nil, nil
	}
	bound := ledger.HistoryBound(s, l.HistoryFloor)
	if !s.FrozenUpper.IsZero() {
		p, err := decodeProgress(s.PageToken)
		if err != nil {
			return nil, err
		}
		p.Phase, p.Token = "messages", ""
		p.MessageLower, p.EventLower = bound, bound
		p.Reported = false
		if s.FrozenUpper.Before(bound) {
			s.FrozenUpper = bound
		}
		s.PageToken = encode(p)
	}
	kept := s.Backlog[:0]
	for _, raw := range s.Backlog {
		var item queuedChange
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			return nil, fmt.Errorf("catchup: invalid durable backlog for %s", name)
		}
		if item.Change.EventTime.IsZero() {
			item.Change.EventTime = item.At
		}
		if item.Malformed == "" && inbound.HistorySkipCause(l, item.Change) != "" {
			inbound.RetireQueuedHistory(l, item.Change)
			added := inbound.RetainHistorySkip(l, cfg, item.Change, now, inbound.HistorySpeaker(l, cfg, item.Change), true, l.IdentityUserID)
			if added && item.Change.ActTime().IsZero() && log != nil {
				log.Warn("inbound act has no usable history time", "space", name, "kind", item.Change.Kind)
			}
			l.Counters["history_backlog_skipped:"+name]++
			continue
		}
		kept = append(kept, raw)
	}
	s.Backlog = kept
	return inbound.RetireSpaceHistory(l, cfg, name, log), nil
}
