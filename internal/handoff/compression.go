package handoff

import (
	bolt "go.etcd.io/bbolt"
	"time"
)

func (e *Engine) BeginCompression(scope, session string, generation uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(familyBucket)
		f, err := lookup(b, scope, session)
		if err != nil {
			return err
		}
		if f == nil {
			return fault("unknown_session")
		}
		if f.Generation != generation {
			return fault("stale_notice")
		}
		if f.Phase == Trial {
			return fault("handoff_in_progress")
		}
		if f.Compressing == nil {
			f.Compressing = map[string]time.Time{}
		}
		f.Compressing[session] = e.now().Add(e.policy.ContextTimeout)
		return put(b, f)
	})
}
func (e *Engine) IsCompressing(scope, session string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	active := false
	err := e.db.View(func(tx *bolt.Tx) error {
		f, err := lookup(tx.Bucket(familyBucket), scope, session)
		if f != nil {
			active = f.Compressing[session].After(e.now())
		}
		return err
	})
	return active, err
}
