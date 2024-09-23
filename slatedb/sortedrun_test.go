package slatedb

import (
	"github.com/oklog/ulid/v2"
	"github.com/samber/mo"
	"github.com/stretchr/testify/assert"
	"github.com/thanos-io/objstore"
	"testing"
)

func buildSRWithSSTs(
	n uint64,
	keysPerSST uint64,
	tableStore *TableStore,
	keyGen OrderedBytesGenerator,
	valGen OrderedBytesGenerator,
) SortedRun {

	sstList := make([]SSTableHandle, 0, n)
	for i := uint64(0); i < n; i++ {
		writer := tableStore.tableWriter(newSSTableIDCompacted(ulid.Make()))
		for j := uint64(0); j < keysPerSST; j++ {
			writer.add(keyGen.next(), mo.Some(valGen.next()))
		}

		sst, _ := writer.close()
		sstList = append(sstList, *sst)
	}

	return SortedRun{0, sstList}
}

func TestOneSstSRIter(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	format := newSSTableFormat(4096, 3, CompressionNone)
	tableStore := newTableStore(bucket, format, "")

	builder := tableStore.tableBuilder()
	builder.add([]byte("key1"), mo.Some([]byte("value1")))
	builder.add([]byte("key2"), mo.Some([]byte("value2")))
	builder.add([]byte("key3"), mo.Some([]byte("value3")))

	encodedSST, err := builder.build()
	assert.NoError(t, err)
	sstHandle, err := tableStore.writeSST(newSSTableIDCompacted(ulid.Make()), encodedSST)
	assert.NoError(t, err)

	sr := SortedRun{0, []SSTableHandle{*sstHandle}}
	iter := newSortedRunIterator(sr, tableStore, 1, 1)
	assertIterNext(t, iter, []byte("key1"), []byte("value1"))
	assertIterNext(t, iter, []byte("key2"), []byte("value2"))
	assertIterNext(t, iter, []byte("key3"), []byte("value3"))

	kv, err := iter.Next()
	assert.NoError(t, err)
	assert.True(t, kv.IsAbsent())
}

func TestManySstSRIter(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	format := newSSTableFormat(4096, 3, CompressionNone)
	tableStore := newTableStore(bucket, format, "")

	builder := tableStore.tableBuilder()
	builder.add([]byte("key1"), mo.Some([]byte("value1")))
	builder.add([]byte("key2"), mo.Some([]byte("value2")))

	encodedSST, err := builder.build()
	assert.NoError(t, err)
	sstHandle, err := tableStore.writeSST(newSSTableIDCompacted(ulid.Make()), encodedSST)
	assert.NoError(t, err)

	builder = tableStore.tableBuilder()
	builder.add([]byte("key3"), mo.Some([]byte("value3")))

	encodedSST, err = builder.build()
	assert.NoError(t, err)
	sstHandle2, err := tableStore.writeSST(newSSTableIDCompacted(ulid.Make()), encodedSST)
	assert.NoError(t, err)

	sr := SortedRun{0, []SSTableHandle{*sstHandle, *sstHandle2}}
	iter := newSortedRunIterator(sr, tableStore, 1, 1)
	assertIterNext(t, iter, []byte("key1"), []byte("value1"))
	assertIterNext(t, iter, []byte("key2"), []byte("value2"))
	assertIterNext(t, iter, []byte("key3"), []byte("value3"))

	kv, err := iter.Next()
	assert.NoError(t, err)
	assert.True(t, kv.IsAbsent())
}

func TestSRIterFromKey(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	format := newSSTableFormat(4096, 3, CompressionNone)
	tableStore := newTableStore(bucket, format, "")

	firstKey := []byte("aaaaaaaaaaaaaaaa")
	keyGen := newOrderedBytesGeneratorWithByteRange(firstKey, byte('a'), byte('z'))
	testCaseKeyGen := keyGen.clone()

	firstVal := []byte("1111111111111111")
	valGen := newOrderedBytesGeneratorWithByteRange(firstVal, byte(1), byte(26))
	testCaseValGen := valGen.clone()

	sr := buildSRWithSSTs(3, 10, tableStore, keyGen, valGen)

	for i := 0; i < 30; i++ {
		expectedKeyGen := testCaseKeyGen.clone()
		expectedValGen := testCaseValGen.clone()
		fromKey := testCaseKeyGen.next()
		testCaseValGen.next()

		kvIter := newSortedRunIteratorFromKey(fromKey, sr, tableStore, 1, 1)

		for j := 0; j < 30-i; j++ {
			assertIterNext(t, kvIter, expectedKeyGen.next(), expectedValGen.next())
		}
		next, err := kvIter.Next()
		assert.NoError(t, err)
		assert.False(t, next.IsPresent())
	}
}

func TestSRIterFromKeyLowerThanRange(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	format := newSSTableFormat(4096, 3, CompressionNone)
	tableStore := newTableStore(bucket, format, "")

	firstKey := []byte("aaaaaaaaaaaaaaaa")
	keyGen := newOrderedBytesGeneratorWithByteRange(firstKey, byte('a'), byte('z'))
	expectedKeyGen := keyGen.clone()

	firstVal := []byte("1111111111111111")
	valGen := newOrderedBytesGeneratorWithByteRange(firstVal, byte(1), byte(26))
	expectedValGen := valGen.clone()

	sr := buildSRWithSSTs(3, 10, tableStore, keyGen, valGen)
	kvIter := newSortedRunIteratorFromKey([]byte("aaaaaaaaaa"), sr, tableStore, 1, 1)

	for j := 0; j < 30; j++ {
		assertIterNext(t, kvIter, expectedKeyGen.next(), expectedValGen.next())
	}
	next, err := kvIter.Next()
	assert.NoError(t, err)
	assert.False(t, next.IsPresent())
}

func TestSRIterFromKeyHigherThanRange(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	format := newSSTableFormat(4096, 3, CompressionNone)
	tableStore := newTableStore(bucket, format, "")

	firstKey := []byte("aaaaaaaaaaaaaaaa")
	keyGen := newOrderedBytesGeneratorWithByteRange(firstKey, byte('a'), byte('z'))

	firstVal := []byte("1111111111111111")
	valGen := newOrderedBytesGeneratorWithByteRange(firstVal, byte(1), byte(26))

	sr := buildSRWithSSTs(3, 10, tableStore, keyGen, valGen)
	kvIter := newSortedRunIteratorFromKey([]byte("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"), sr, tableStore, 1, 1)
	next, err := kvIter.Next()
	assert.NoError(t, err)
	assert.False(t, next.IsPresent())
}
