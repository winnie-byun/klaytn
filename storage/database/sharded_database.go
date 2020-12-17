// Copyright 2019 The klaytn Authors
// This file is part of the klaytn library.
//
// The klaytn library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The klaytn library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the klaytn library. If not, see <http://www.gnu.org/licenses/>.

package database

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"time"
)

var errKeyLengthZero = fmt.Errorf("database key for sharded database should be greater than 0")

type shardedDB struct {
	fn        string
	shards    []Database
	numShards uint

	pdbBatchTaskCh chan pdbBatchTask
}

type pdbBatchTask struct {
	batch    Batch               // A batch that each worker executes.
	index    int                 // Index of given batch.
	resultCh chan pdbBatchResult // Batch result channel for each shardedDBBatch.
}

type pdbBatchResult struct {
	index int   // Index of the batch result.
	err   error // Error from the batch write operation.
}

// newShardedDB creates database with numShards shards, or partitions.
// The type of database is specified DBConfig.DBType.
func newShardedDB(dbc *DBConfig, et DBEntryType, numShards uint) (*shardedDB, error) {
	const numShardsLimit = 16

	if numShards == 0 {
		logger.Crit("numShards should be greater than 0!")
	}

	if numShards > numShardsLimit {
		logger.Crit(fmt.Sprintf("numShards should be equal to or smaller than %v, but it is %v.", numShardsLimit, numShards))
	}

	if !IsPow2(numShards) {
		logger.Crit(fmt.Sprintf("numShards should be power of two, but it is %v", numShards))
	}

	shards := make([]Database, 0, numShards)
	pdbBatchTaskCh := make(chan pdbBatchTask, numShards*2)
	for i := 0; i < int(numShards); i++ {
		copiedDBC := *dbc
		copiedDBC.Dir = path.Join(copiedDBC.Dir, strconv.Itoa(i))
		copiedDBC.LevelDBCacheSize /= int(numShards)

		db, err := newDatabase(&copiedDBC, et)
		if err != nil {
			return nil, err
		}
		shards = append(shards, db)
		go batchWriteWorker(pdbBatchTaskCh)
	}

	return &shardedDB{
		fn: dbc.Dir, shards: shards,
		numShards: numShards, pdbBatchTaskCh: pdbBatchTaskCh}, nil
}

// batchWriteWorker executes passed batch tasks.
func batchWriteWorker(batchTasks <-chan pdbBatchTask) {
	for task := range batchTasks {
		task.resultCh <- pdbBatchResult{task.index, task.batch.Write()}
	}
}

// IsPow2 checks if the given number is power of two or not.
func IsPow2(num uint) bool {
	return (num & (num - 1)) == 0
}

// shardIndexByKey returns shard index derived from the given key.
// If len(key) is zero, it returns errKeyLengthZero.
func shardIndexByKey(key []byte, numShards uint) (int, error) {
	if len(key) == 0 {
		return 0, errKeyLengthZero
	}

	return int(key[0]) & (int(numShards) - 1), nil
}

// getShardByKey returns the shard corresponding to the given key.
func (pdb *shardedDB) getShardByKey(key []byte) (Database, error) {
	if shardIndex, err := shardIndexByKey(key, uint(pdb.numShards)); err != nil {
		return nil, err
	} else {
		return pdb.shards[shardIndex], nil
	}
}

func (pdb *shardedDB) Put(key []byte, value []byte) error {
	if shard, err := pdb.getShardByKey(key); err != nil {
		return err
	} else {
		return shard.Put(key, value)
	}
}

func (pdb *shardedDB) Get(key []byte) ([]byte, error) {
	if shard, err := pdb.getShardByKey(key); err != nil {
		return nil, err
	} else {
		return shard.Get(key)
	}
}

func (pdb *shardedDB) Has(key []byte) (bool, error) {
	if shard, err := pdb.getShardByKey(key); err != nil {
		return false, err
	} else {
		return shard.Has(key)
	}
}

func (pdb *shardedDB) Delete(key []byte) error {
	if shard, err := pdb.getShardByKey(key); err != nil {
		return err
	} else {
		return shard.Delete(key)
	}
}

func (pdb *shardedDB) Close() {
	close(pdb.pdbBatchTaskCh)

	for _, shard := range pdb.shards {
		shard.Close()
	}
}

// Not enough size of channel slows down the iterator
const shardedDBCombineChanSize = 1024 // Size of resultCh
const shardedDBSubChannelSize = 128   // Size of each channel of resultChs

// shardedDBIterator iterates all items of each shardDB.
// This is useful when you want to get items in serial.
type shardedDBIterator struct {
	shardedDBChanIterator

	resultCh chan entry
	key      []byte // current key
	value    []byte // current value
}

// NewIterator creates a iterator over the entire keyspace contained within
// the key-value database.
// If you want to get items in parallel from channels, checkout shardedDB.NewChanIterator()
func (pdb *shardedDB) NewIterator() Iterator {
	return pdb.newIterator(func(db Database) Iterator { return db.NewIterator() })
}

// NewIteratorWithStart creates a iterator over a subset of database content
// starting at a particular initial key (or after, if it does not exist).
func (pdb *shardedDB) NewIteratorWithStart(start []byte) Iterator {
	return pdb.newIterator(func(db Database) Iterator { return db.NewIteratorWithStart(start) })
}

// NewIteratorWithPrefix creates a iterator over a subset of database content
// with a particular key prefix.
func (pdb *shardedDB) NewIteratorWithPrefix(prefix []byte) Iterator {
	return pdb.newIterator(func(db Database) Iterator { return db.NewIteratorWithPrefix(prefix) })
}

func (pdb *shardedDB) newIterator(newIterator func(Database) Iterator) Iterator {
	it := &shardedDBIterator{
		pdb.NewChanIterator(context.Background(), newIterator),
		make(chan entry, shardedDBCombineChanSize),
		nil, nil}

	go it.newCombineWorker()

	return it
}

func (it shardedDBIterator) newCombineWorker() {
	// TODO Put all items in biary-alphabetical order.
	for {
	chanIter:
		for i, ch := range it.resultChs {
			select {
			case <-it.ctx.Done():
				logger.Trace("[shardedDBIterator] combine worker ends due to ctx")
				close(it.resultCh)
				return
			case e, ok := <-ch:
				if !ok {
					it.resultChs = append(it.resultChs[:i], it.resultChs[i+1:]...)
					break chanIter
				}
				it.resultCh <- e
			default:
			}
		}
		if len(it.resultChs) == 0 {
			logger.Trace("[shardedDBIterator] combine worker finishes iterating")
			close(it.resultCh)
			return
		}
	}
}

// Next gets the next item from iterators.
// The first call of Next() might take 50ms.
func (pdi *shardedDBIterator) Next() bool {
	maxRetry := 5
	for i := 0; i < maxRetry; i++ {
		select {
		case e, ok := <-pdi.resultCh:
			if ok {
				pdi.key, pdi.value = e.key, e.val
				return true
			} else {
				logger.Error("[shardedDBIterator] Next is called on closed channel")
				return false
			}
		default:
			logger.Debug("[shardedDBIterator] no value is ready")
			// shardedDBIterator needs some time for workers to fill channels
			// If there is no sleep here, there should a sleep after NewIterator()
			time.Sleep(10 * time.Millisecond)
		}
	}
	logger.Error("[shardedDBIterator] Next() takes more than 50ms on unclosed subchannels")
	return false
}

func (pdi *shardedDBIterator) Error() error {
	var err error
	for i, iter := range pdi.iterators {
		if iter.Error() != nil {
			logger.Error("[shardedDBIterator] error from iterator",
				"err", err, "shardNum", i, "key", pdi.key, "val", pdi.value)
			err = iter.Error()
		}
	}
	return err
}

func (pdi *shardedDBIterator) Key() []byte {
	return pdi.key
}

func (pdi *shardedDBIterator) Value() []byte {
	return pdi.value
}

func (pdi *shardedDBIterator) Release() {
	pdi.ctx.Done()
}

type entry struct {
	key, val []byte
}

// shardedDBChanIterator creates iterators for each shard DB.
// Channels subscribing each iterators can be gained.
// This is useful when you want to operate on each items in parallel.
type shardedDBChanIterator struct {
	ctx context.Context

	iterators []Iterator
	resultChs []chan entry
}

// NewChanIterator creates iterators for each shard DB.
// This is useful when you want to operate on each items in parallel.
// If you want to get items in serial, checkout shardedDB.NewIterator()
func (pdb *shardedDB) NewChanIterator(ctx context.Context, newIterator func(Database) Iterator) shardedDBChanIterator {
	it := shardedDBChanIterator{ctx,
		make([]Iterator, len(pdb.shards)),
		make([]chan entry, len(pdb.shards))}

	if it.ctx == nil {
		it.ctx = context.Background()
	}

	for i := 0; i < len(pdb.shards); i++ {
		it.iterators[i] = newIterator(pdb.shards[i])
		it.resultChs[i] = make(chan entry, shardedDBSubChannelSize)
		go it.newChanWorker(it.iterators[i], it.resultChs[i], ctx)
	}

	return it
}

func (*shardedDBChanIterator) newChanWorker(it Iterator, resultCh chan entry, ctx context.Context) {
	for it.Next() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		key := make([]byte, len(it.Key()))
		val := make([]byte, len(it.Value()))
		copy(key, it.Key())
		copy(val, it.Value())
		resultCh <- entry{key, val}
	}
	close(resultCh)
}

// Channels returns channels that can subscribe on.
func (it *shardedDBChanIterator) Channels() []chan entry {
	return it.resultChs
}

// Release stops all iterators, channels and workers
func (it *shardedDBChanIterator) Release() {
	for _, i := range it.iterators {
		i.Release()
	}
	it.ctx.Done()
}

func (pdb *shardedDB) NewBatch() Batch {
	batches := make([]Batch, 0, pdb.numShards)
	for i := 0; i < int(pdb.numShards); i++ {
		batches = append(batches, pdb.shards[i].NewBatch())
	}

	return &shardedDBBatch{batches: batches, numBatches: pdb.numShards,
		taskCh: pdb.pdbBatchTaskCh, resultCh: make(chan pdbBatchResult, pdb.numShards)}
}

func (pdb *shardedDB) Type() DBType {
	return ShardedDB
}

func (pdb *shardedDB) Meter(prefix string) {
	for index, shard := range pdb.shards {
		shard.Meter(prefix + strconv.Itoa(index))
	}
}

type shardedDBBatch struct {
	batches    []Batch
	numBatches uint

	taskCh   chan pdbBatchTask
	resultCh chan pdbBatchResult
}

func (pdbBatch *shardedDBBatch) Put(key []byte, value []byte) error {
	if ShardIndex, err := shardIndexByKey(key, uint(pdbBatch.numBatches)); err != nil {
		return err
	} else {
		return pdbBatch.batches[ShardIndex].Put(key, value)
	}
}

// ValueSize is called to determine whether to write batches when it exceeds
// certain limit. shardedDB returns the largest size of its batches to
// write all batches at once when one of batch exceeds the limit.
func (pdbBatch *shardedDBBatch) ValueSize() int {
	maxSize := 0
	for _, batch := range pdbBatch.batches {
		if batch.ValueSize() > maxSize {
			maxSize = batch.ValueSize()
		}
	}
	return maxSize
}

// Write passes the list of batch tasks to taskCh so batch can be processed
// by underlying workers. Write waits until all workers return the result.
func (pdbBatch *shardedDBBatch) Write() error {
	for index, batch := range pdbBatch.batches {
		pdbBatch.taskCh <- pdbBatchTask{batch, index, pdbBatch.resultCh}
	}

	var err error
	for range pdbBatch.batches {
		if batchResult := <-pdbBatch.resultCh; batchResult.err != nil {
			logger.Error("Error while writing sharded batch", "index", batchResult.index, "err", batchResult.err)
			err = batchResult.err
		}
	}
	// Leave logs for each error but only return the last one.
	return err
}

func (pdbBatch *shardedDBBatch) Reset() {
	for _, batch := range pdbBatch.batches {
		batch.Reset()
	}
}
