/*===============================================================================
  3) pkg/storage/batcher.go 
  -----------------------------------------------------------------------------*/

	package storage

	import (
			"time"
			bolt "go.etcd.io/bbolt"
	)
	
	type kv struct{ k, v []byte }
	
	type Batcher struct {
			db     *bolt.DB
			bucket string
			ch     chan kv
	}
	
	func NewBatcher(db *bolt.DB, bucket string) *Batcher {
			b := &Batcher{db: db, bucket: bucket, ch: make(chan kv, 1024)}
			go b.loop()
			return b
	}
	
	func (b *Batcher) Put(k, v []byte) { b.ch <- kv{k, v} }
	
	func (b *Batcher) loop() {
			buf := make([]kv, 0, 100)
			flush := func() {
					if len(buf) == 0 { return }
					_ = b.db.Update(func(tx *bolt.Tx) error {
							bk := tx.Bucket([]byte(b.bucket))
							for _, p := range buf { bk.Put(p.k, p.v) }
							return nil
					})
					buf = buf[:0]
			}
			ticker := time.NewTicker(250*time.Millisecond)
			for {
					select {
					case p := <-b.ch:
							buf = append(buf, p)
							if len(buf) >= 100 { flush() }
					case <-ticker.C:
							flush()
					}
			}
	}
	