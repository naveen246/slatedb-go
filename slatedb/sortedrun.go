package slatedb

import (
	"bytes"
	"github.com/samber/mo"
)

// ------------------------------------------------
// SortedRun
// ------------------------------------------------

type SortedRun struct {
	id      uint32
	sstList []SSTableHandle
}

func (s *SortedRun) indexOfSSTWithKey(key []byte) mo.Option[int] {
	index := 0
	for i, sst := range s.sstList {
		firstKey := sst.info.borrow().FirstKeyBytes()
		if bytes.Compare(firstKey, key) > 0 {
			index = i
			break
		} else if i == len(s.sstList)-1 {
			index = i + 1
			break
		}
	}
	if index > 0 {
		return mo.Some(index - 1)
	}
	return mo.None[int]()
}

func (s *SortedRun) sstWithKey(key []byte) mo.Option[SSTableHandle] {
	index, ok := s.indexOfSSTWithKey(key).Get()
	if ok {
		return mo.Some(s.sstList[index])
	}
	return mo.None[SSTableHandle]()
}

// ------------------------------------------------
// SortedRunIterator
// ------------------------------------------------

type SortedRunIterator struct {
	currentKVIter     mo.Option[*SSTIterator]
	sstListIter       *SSTListIterator
	tableStore        *TableStore
	numBlocksToFetch  uint64
	numBlocksToBuffer uint64
}

func newSortedRunIterator(
	sortedRun SortedRun,
	tableStore *TableStore,
	maxFetchTasks uint64,
	numBlocksToFetch uint64,
) *SortedRunIterator {
	return newSortedRunIter(sortedRun.sstList, tableStore, maxFetchTasks, numBlocksToFetch, mo.None[[]byte]())
}

func newSortedRunIteratorFromKey(
	key []byte,
	sortedRun SortedRun,
	tableStore *TableStore,
	maxFetchTasks uint64,
	numBlocksToFetch uint64,
) *SortedRunIterator {
	sstList := sortedRun.sstList
	idx, ok := sortedRun.indexOfSSTWithKey(key).Get()
	if ok {
		sstList = sortedRun.sstList[idx:]
	}

	return newSortedRunIter(sstList, tableStore, maxFetchTasks, numBlocksToFetch, mo.Some(key))
}

func newSortedRunIter(
	sstList []SSTableHandle,
	tableStore *TableStore,
	maxFetchTasks uint64,
	numBlocksToFetch uint64,
	fromKey mo.Option[[]byte],
) *SortedRunIterator {

	sstListIter := newSSTListIterator(sstList)
	currentKVIter := mo.None[*SSTIterator]()
	sst, ok := sstListIter.Next()
	if ok {
		var iter *SSTIterator
		if fromKey.IsPresent() {
			key, _ := fromKey.Get()
			iter = newSSTIteratorFromKey(&sst, tableStore, key, maxFetchTasks, numBlocksToFetch)
		} else {
			iter = newSSTIterator(&sst, tableStore, maxFetchTasks, numBlocksToFetch)
		}

		currentKVIter = mo.Some(iter)
	}

	return &SortedRunIterator{
		currentKVIter:     currentKVIter,
		sstListIter:       sstListIter,
		tableStore:        tableStore,
		numBlocksToFetch:  maxFetchTasks,
		numBlocksToBuffer: numBlocksToFetch,
	}
}

func (iter *SortedRunIterator) Next() (mo.Option[KeyValue], error) {
	for {
		kvDel, err := iter.NextEntry()
		if err != nil {
			return mo.None[KeyValue](), err
		}
		keyVal, ok := kvDel.Get()
		if ok {
			if keyVal.valueDel.isTombstone {
				continue
			}

			return mo.Some[KeyValue](KeyValue{
				key:   keyVal.key,
				value: keyVal.valueDel.value,
			}), nil
		} else {
			return mo.None[KeyValue](), nil
		}
	}
}

func (iter *SortedRunIterator) NextEntry() (mo.Option[KeyValueDeletable], error) {
	for {
		if iter.currentKVIter.IsAbsent() {
			return mo.None[KeyValueDeletable](), nil
		}

		kvIter, _ := iter.currentKVIter.Get()
		next, err := kvIter.NextEntry()
		if err != nil {
			return mo.None[KeyValueDeletable](), err
		}

		if next.IsPresent() {
			kv, _ := next.Get()
			return mo.Some(kv), nil
		}

		sst, ok := iter.sstListIter.Next()
		if !ok {
			return mo.None[KeyValueDeletable](), nil
		}
		newKVIter := newSSTIterator(&sst, iter.tableStore, iter.numBlocksToFetch, iter.numBlocksToBuffer)
		iter.currentKVIter = mo.Some(newKVIter)
	}
}

// ------------------------------------------------
// SSTListIterator
// ------------------------------------------------

type SSTListIterator struct {
	sstList []SSTableHandle
	current int
}

func newSSTListIterator(sstList []SSTableHandle) *SSTListIterator {
	return &SSTListIterator{sstList, 0}
}

func (iter *SSTListIterator) Next() (SSTableHandle, bool) {
	if iter.current >= len(iter.sstList) {
		return SSTableHandle{}, false
	}
	sst := iter.sstList[iter.current]
	iter.current++
	return sst, true
}
