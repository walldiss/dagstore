package dagstore

import (
	"context"
	"fmt"

	"github.com/filecoin-project/dagstore/mount"
	"github.com/ipld/go-car/v2"
)

//
// This file contains methods that are called from the event loop
// but are run asynchronously in dedicated goroutines.
//

// acquireAsync acquires a shard by fetching its data, obtaining its index, and
// joining them to form a ShardAccessor.
func (d *DAGStore) acquireAsync(ctx context.Context, w *waiter, s *Shard, mnt mount.Mount) {
	k := s.key
	reader, err := mnt.Fetch(ctx)
	if err != nil {
		// release the shard to decrement the refcount that's incremented before `acquireAsync` is called.
		_ = d.queueTask(&task{op: OpShardRelease, shard: s}, d.completionCh)

		// fail the shard
		_ = d.queueTask(&task{op: OpShardFail, shard: s, err: fmt.Errorf("failed to acquire reader of mount: %w", err)}, d.completionCh)

		// send the shard error to the caller.
		d.sendResult(&ShardResult{Key: k, Error: err}, w)
		return
	}

	idx, err := d.indices.GetFullIndex(k)
	if err != nil {
		if err := reader.Close(); err != nil {
			log.Errorf("failed to close mount reader: %s", err)
		}

		// release the shard to decrement the refcount that's incremented before `acquireAsync` is called.
		_ = d.queueTask(&task{op: OpShardRelease, shard: s}, d.completionCh)

		// fail the shard
		_ = d.queueTask(&task{op: OpShardFail, shard: s, err: fmt.Errorf("failed to recover index for shard %s: %w", k, err)}, d.completionCh)

		// send the shard error to the caller.
		d.sendResult(&ShardResult{Key: k, Error: err}, w)
		return
	}

	sa, err := NewShardAccessor(reader, idx, s)

	// send the shard accessor to the caller.
	d.sendResult(&ShardResult{Key: k, Accessor: sa, Error: err}, w)
}

// initializeAsync initializes a shard asynchronously by fetching its data and
// performing indexing.
func (d *DAGStore) initializeAsync(ctx context.Context, s *Shard, mnt mount.Mount) {
	reader, err := mnt.Fetch(ctx)
	if err != nil {
		_ = d.failShard(s, fmt.Errorf("failed to acquire reader of mount: %w", err), d.completionCh)
		return
	}
	defer reader.Close()

	// works for both CARv1 and CARv2.
	// TODO avoid using this API since it's too opaque; if an inline index
	//  exists, this API returns quickly, if not, an index will be generated
	//  which is a costly operation in terms of IO and wall clock time. The DAG
	//  store will need to have control over scheduling of index generation.
	//  https://github.com/filecoin-project/dagstore/issues/50
	idx, err := car.ReadOrGenerateIndex(reader)
	if err != nil {
		_ = d.failShard(s, fmt.Errorf("failed to read/generate CAR Index: %w", err), d.completionCh)
		return
	}
	if err := d.indices.AddFullIndex(s.key, idx); err != nil {
		_ = d.failShard(s, fmt.Errorf("failed to add index for shard: %w", err), d.completionCh)
		return
	}

	_ = d.queueTask(&task{op: OpShardMakeAvailable, shard: s}, d.completionCh)
}

func (d *DAGStore) failShard(s *Shard, err error, ch chan *task) error {
	return d.queueTask(&task{op: OpShardFail, shard: s, err: err}, ch)
}
