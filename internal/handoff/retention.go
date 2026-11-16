package handoff

import (
	"encoding/json"
	bolt "go.etcd.io/bbolt"
	"sort"
	"time"
)

func trimReplays(b *bolt.Bucket, now time.Time) error {
	var rows []*Family
	total := 0
	err := b.ForEach(func(_, raw []byte) error {
		var f Family
		if err := json.Unmarshal(raw, &f); err != nil {
			return err
		}
		if len(f.Replay) > 0 {
			rows = append(rows, &f)
			total += len(f.Replay)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ReplayUntil.Before(rows[j].ReplayUntil) })
	for _, f := range rows {
		if f.ReplayUntil.After(now) && total <= 63<<20 {
			continue
		}
		total -= len(f.Replay)
		f.Replay = nil
		f.ReplayRequest = ""
		f.ReplayFingerprint = ""
		if err := put(b, f); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) sweeper() {
	defer close(e.stopped)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-tick.C:
			e.mu.Lock()
			if !e.closed {
				_ = e.db.Update(func(tx *bolt.Tx) error { return trimReplays(tx.Bucket(familyBucket), e.now()) })
			}
			e.mu.Unlock()
		}
	}
}
