package slatedb

import (
	"bytes"
	"context"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	assert2 "github.com/slatedb/slatedb-go/internal/assert"
	"github.com/slatedb/slatedb-go/internal/compress"
	"github.com/slatedb/slatedb-go/internal/sstable"
	"github.com/slatedb/slatedb-go/internal/types"
	"github.com/slatedb/slatedb-go/slatedb/config"
	"github.com/slatedb/slatedb-go/slatedb/state"
	"github.com/slatedb/slatedb-go/slatedb/store"

	"github.com/stretchr/testify/assert"
	"github.com/thanos-io/objstore"

	"github.com/slatedb/slatedb-go/slatedb/common"
)

// TODO: Look into injecting failpoints
//  https://pkg.go.dev/github.com/pingcap/failpoint ?

func TestPutGetDelete(t *testing.T) {
	ctx := context.Background()
	bucket := objstore.NewInMemBucket()
	db, err := OpenWithOptions(context.Background(), "/tmp/test_kv_store", bucket, testDBOptions(0, 1024))
	require.NoError(t, err)
	defer db.Close()

	key := []byte("key1")
	value := []byte("value1")
	db.Put(key, value)
	val, err := db.Get(context.Background(), key)
	require.NoError(t, err)
	assert.Equal(t, value, val)

	key = []byte("key2")
	value = []byte("value2")
	db.Put(key, value)
	err = db.FlushWAL()
	require.NoError(t, err)
	val, err = db.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, value, val)

	db.Delete(key)
	_, err = db.Get(ctx, key)
	assert.ErrorIs(t, err, common.ErrKeyNotFound)
}

func TestGetNonExistingKey(t *testing.T) {
	ctx := context.Background()
	bucket := objstore.NewInMemBucket()
	db, err := OpenWithOptions(ctx, "/tmp/test_kv_store", bucket, config.DefaultDBOptions())
	require.NoError(t, err)
	defer db.Close()

	db.Put([]byte("key1"), []byte("value1"))
	require.NoError(t, db.FlushWAL())
	require.NoError(t, db.FlushMemtableToL0())

	_, err = db.Get(ctx, []byte("key2"))
	assert.ErrorIs(t, err, common.ErrKeyNotFound)
}

func TestGetWithNonDurableWritesAndFlushToL0(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	db, err := OpenWithOptions(context.Background(), "/tmp/test_kv_store", bucket, config.DefaultDBOptions())
	require.NoError(t, err)
	defer db.Close()

	db.PutWithOptions([]byte("k1"), []byte("v1"), config.WriteOptions{AwaitDurable: false})
	require.NoError(t, db.FlushWAL())
	require.NoError(t, db.FlushMemtableToL0())

	db.PutWithOptions([]byte("k0"), []byte("v0"), config.WriteOptions{AwaitDurable: false})
	require.NoError(t, db.FlushWAL())
	require.NoError(t, db.FlushMemtableToL0())

	data, err := db.GetWithOptions(context.Background(), []byte("k1"), config.ReadOptions{ReadLevel: config.Committed})
	assert.Equal(t, "v1", string(data))
	require.NoError(t, err)

	data, err = db.GetWithOptions(context.Background(), []byte("k1"), config.ReadOptions{ReadLevel: config.Uncommitted})
	assert.Equal(t, "v1", string(data))
	require.NoError(t, err)

	data, err = db.GetWithOptions(context.Background(), []byte("k0"), config.ReadOptions{ReadLevel: config.Committed})
	assert.Equal(t, "v0", string(data))
	require.NoError(t, err)

	data, err = db.GetWithOptions(context.Background(), []byte("k0"), config.ReadOptions{ReadLevel: config.Uncommitted})
	assert.Equal(t, "v0", string(data))
	require.NoError(t, err)
}

func TestPutFlushesMemtable(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	dbPath := "/tmp/test_kv_store"
	db, err := OpenWithOptions(context.Background(), dbPath, bucket, testDBOptions(0, 128))
	require.NoError(t, err)
	defer db.Close()

	manifestStore := store.NewManifestStore(dbPath, bucket)
	stored, err := store.LoadStoredManifest(manifestStore)
	require.NoError(t, err)
	assert.True(t, stored.IsPresent())

	storedManifest, _ := stored.Get()
	conf := sstable.DefaultConfig()
	conf.MinFilterKeys = 10
	tableStore := store.NewTableStore(bucket, conf, dbPath)

	lastCompacted := uint64(0)
	for i := 0; i < 3; i++ {
		key := repeatedChar(rune('a'+i), 16)
		value := repeatedChar(rune('b'+i), 50)
		db.Put(key, value)

		key = repeatedChar(rune('j'+i), 16)
		value = repeatedChar(rune('k'+i), 50)
		db.Put(key, value)

		dbState := waitForManifestCondition(storedManifest, time.Second*30, func(state *state.CoreStateSnapshot) bool {
			return state.LastCompactedWalSSTID.Load() > lastCompacted
		})
		assert.Equal(t, uint64(i*2+2), dbState.LastCompactedWalSSTID.Load())
		lastCompacted = dbState.LastCompactedWalSSTID.Load()
	}

	dbState, err := storedManifest.Refresh()
	require.NoError(t, err)
	l0 := dbState.L0
	ctx := context.Background()
	assert.Equal(t, 3, len(l0))
	for i := 0; i < 3; i++ {
		sst := l0[2-i]
		iter, err := sstable.NewIterator(&sst, tableStore)
		require.NoError(t, err)

		kv, ok := iter.Next(ctx)
		assert.True(t, ok)
		key := repeatedChar(rune('a'+i), 16)
		value := repeatedChar(rune('b'+i), 50)
		assert.Equal(t, key, kv.Key)
		assert.Equal(t, value, kv.Value)

		kv, ok = iter.Next(ctx)
		assert.True(t, ok)
		key = repeatedChar(rune('j'+i), 16)
		value = repeatedChar(rune('k'+i), 50)
		assert.Equal(t, key, kv.Key)
		assert.Equal(t, value, kv.Value)

		kv, ok = iter.Next(ctx)
		assert.False(t, ok)
		assert.Equal(t, types.KeyValue{}, kv)
	}
}

func TestPutEmptyValue(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	db, err := OpenWithOptions(context.Background(), "/tmp/test_kv_store", bucket, testDBOptions(0, 1024))
	require.NoError(t, err)
	defer db.Close()

	key := []byte("key1")
	value := []byte("")
	db.Put(key, value)
	err = db.FlushWAL()
	require.NoError(t, err)
	val, err := db.Get(context.Background(), key)
	require.NoError(t, err)
	assert.Equal(t, value, val)
}

func TestFlushWhileIterating(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	db, err := OpenWithOptions(context.Background(), "/tmp/test_kv_store", bucket, testDBOptions(0, 1024))
	require.NoError(t, err)
	defer db.Close()

	wal := db.state.WAL()
	wal.Put([]byte("abc1111"), []byte("value1111"))
	wal.Put([]byte("abc2222"), []byte("value2222"))
	wal.Put([]byte("abc3333"), []byte("value3333"))

	iter := wal.Iter()

	next, err := iter.Next()
	require.NoError(t, err)
	kv, _ := next.Get()
	assert.Equal(t, []byte("abc1111"), kv.Key)
	assert.Equal(t, []byte("value1111"), kv.Value)

	err = db.FlushWAL()
	require.NoError(t, err)

	next, err = iter.Next()
	require.NoError(t, err)
	kv, _ = next.Get()
	assert.Equal(t, []byte("abc2222"), kv.Key)
	assert.Equal(t, []byte("value2222"), kv.Value)

	next, err = iter.Next()
	require.NoError(t, err)
	kv, _ = next.Get()
	assert.Equal(t, []byte("abc3333"), kv.Key)
	assert.Equal(t, []byte("value3333"), kv.Value)
}

func TestFlushMemtableToL0(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	dbPath := "/tmp/test_kv_store"
	db, err := OpenWithOptions(context.Background(), dbPath, bucket, config.DefaultDBOptions())
	require.NoError(t, err)
	defer db.Close()

	kvPairs := []types.KeyValue{
		{Key: []byte("abc1111"), Value: []byte("value1111")},
		{Key: []byte("abc2222"), Value: []byte("value2222")},
		{Key: []byte("abc3333"), Value: []byte("value3333")},
	}

	// write KeyValue pairs to DB and call db.FlushWAL()
	for _, kv := range kvPairs {
		db.Put(kv.Key, kv.Value)
	}
	err = db.FlushWAL()
	require.NoError(t, err)

	// verify that WAL is empty after FlushWAL() is called
	assert.Equal(t, int64(0), db.state.WAL().Size())
	assert.Equal(t, 0, db.state.ImmWALs().Len())

	// verify that all KeyValue pairs are present in Memtable
	memtable := db.state.Memtable()
	for _, kv := range kvPairs {
		assert.True(t, memtable.Get(kv.Key).IsPresent())
	}

	err = db.FlushMemtableToL0()
	require.NoError(t, err)

	// verify that Memtable is empty after FlushMemtableToL0()
	assert.Equal(t, int64(0), db.state.Memtable().Size())

	// verify that we can read keys from Level0
	for _, kv := range kvPairs {
		val, err := db.Get(context.Background(), kv.Key)
		require.NoError(t, err)
		assert.True(t, bytes.Equal(kv.Value, val))
	}
}

func TestBasicRestore(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	dbPath := "/tmp/test_kv_store"
	db, err := OpenWithOptions(context.Background(), dbPath, bucket, testDBOptions(0, 128))
	require.NoError(t, err)

	// do a few writes that will result in l0 flushes
	l0Count := 3
	for i := 0; i < l0Count; i++ {
		key := repeatedChar(rune('a'+i), 16)
		value := repeatedChar(rune('b'+i), 48)
		db.Put(key, value)
		key = repeatedChar(rune('j'+i), 16)
		value = repeatedChar(rune('k'+i), 48)
		db.Put(key, value)
	}

	// write some smaller keys so that we populate wal without flushing to l0
	sstCount := 5
	for i := 0; i < sstCount; i++ {
		db.Put([]byte(strconv.Itoa(i)), []byte(strconv.Itoa(i)))
		err := db.FlushWAL()
		require.NoError(t, err)
	}
	db.Close()

	// recover and validate that sst files are loaded on recovery.
	dbRestored, err := OpenWithOptions(context.Background(), dbPath, bucket, testDBOptions(0, 128))
	require.NoError(t, err)
	defer dbRestored.Close()

	for i := 0; i < l0Count; i++ {
		val, err := dbRestored.Get(context.Background(), repeatedChar(rune('a'+i), 16))
		require.NoError(t, err)
		assert.Equal(t, repeatedChar(rune('b'+i), 48), val)
		val, err = dbRestored.Get(context.Background(), repeatedChar(rune('j'+i), 16))
		require.NoError(t, err)
		assert.Equal(t, repeatedChar(rune('k'+i), 48), val)
	}
	for i := 0; i < sstCount; i++ {
		val, err := dbRestored.Get(context.Background(), []byte(strconv.Itoa(i)))
		require.NoError(t, err)
		assert.Equal(t, []byte(strconv.Itoa(i)), val)
	}

	manifestStore := store.NewManifestStore(dbPath, bucket)
	stored, err := store.LoadStoredManifest(manifestStore)
	require.NoError(t, err)
	assert.True(t, stored.IsPresent())

	storedManifest, _ := stored.Get()
	dbState := storedManifest.DbState()
	assert.Equal(t, uint64(sstCount+2*l0Count+1), dbState.NextWalSstID.Load())
}

func TestShouldReadUncommittedIfReadLevelUncommitted(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	dbPath := "/tmp/test_kv_store"
	db, err := OpenWithOptions(context.Background(), dbPath, bucket, testDBOptions(0, 1024))
	require.NoError(t, err)
	defer db.Close()

	// we do not wait till WAL is flushed to object store and memtable
	db.PutWithOptions([]byte("foo"), []byte("bar"), config.WriteOptions{AwaitDurable: false})

	value, err := db.GetWithOptions(context.Background(), []byte("foo"), config.ReadOptions{ReadLevel: config.Uncommitted})
	require.NoError(t, err)
	assert.Equal(t, []byte("bar"), value)
}

func TestShouldReadOnlyCommittedData(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	dbPath := "/tmp/test_kv_store"
	db, err := OpenWithOptions(context.Background(), dbPath, bucket, testDBOptions(0, 1024))
	require.NoError(t, err)
	defer db.Close()

	db.Put([]byte("foo"), []byte("bar"))
	db.PutWithOptions([]byte("foo"), []byte("bla"), config.WriteOptions{AwaitDurable: false})

	value, err := db.Get(context.Background(), []byte("foo"))
	require.NoError(t, err)
	assert.Equal(t, []byte("bar"), value)

	value, err = db.GetWithOptions(context.Background(), []byte("foo"), config.ReadOptions{ReadLevel: config.Uncommitted})
	require.NoError(t, err)
	assert.Equal(t, []byte("bla"), value)
}

func TestShouldDeleteWithoutAwaitingFlush(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	dbPath := "/tmp/test_kv_store"
	db, err := OpenWithOptions(context.Background(), dbPath, bucket, testDBOptions(0, 1024))
	require.NoError(t, err)
	defer db.Close()

	db.Put([]byte("foo"), []byte("bar"))
	db.DeleteWithOptions([]byte("foo"), config.WriteOptions{AwaitDurable: false})

	value, err := db.Get(context.Background(), []byte("foo"))
	require.NoError(t, err)
	assert.Equal(t, []byte("bar"), value)

	_, err = db.GetWithOptions(context.Background(), []byte("foo"), config.ReadOptions{ReadLevel: config.Uncommitted})
	assert.ErrorIs(t, err, common.ErrKeyNotFound)
}

func TestSnapshotState(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	dbPath := "/tmp/test_kv_store"
	db, err := OpenWithOptions(context.Background(), dbPath, bucket, testDBOptions(0, 128))
	require.NoError(t, err)

	// write a few keys that will result in memtable flushes
	key1, value1 := repeatedChar('a', 32), repeatedChar('b', 96)
	key2, value2 := repeatedChar('c', 32), repeatedChar('d', 96)
	db.Put(key1, value1)
	db.Put(key2, value2)
	db.Close()

	db, err = OpenWithOptions(context.Background(), dbPath, bucket, testDBOptions(0, 128))
	require.NoError(t, err)
	defer db.Close()
	snapshot := db.state.Snapshot()
	assert.Equal(t, uint64(2), snapshot.Core.LastCompactedWalSSTID.Load())
	assert.Equal(t, uint64(3), snapshot.Core.NextWalSstID.Load())
	assert.Equal(t, 2, len(snapshot.Core.L0))

	val1, err := db.Get(context.Background(), key1)
	require.NoError(t, err)
	assert.Equal(t, value1, val1)

	val2, err := db.Get(context.Background(), key2)
	require.NoError(t, err)
	assert.Equal(t, value2, val2)
}

func TestShouldReadFromCompactedDB(t *testing.T) {
	options := testDBOptionsCompactor(
		0,
		127,
		&config.CompactorOptions{
			PollInterval: 100 * time.Millisecond,
			MaxSSTSize:   256,
		},
	)
	doTestShouldReadCompactedDB(t, options)
	doTestDeleteAndWaitForCompaction(t, options)
}

func TestShouldReadFromCompactedDBNoFilters(t *testing.T) {
	opts := testDBOptionsCompactor(
		math.MaxUint32,
		127,
		&config.CompactorOptions{
			PollInterval: 100 * time.Millisecond,
			MaxSSTSize:   256,
		},
	)
	doTestShouldReadCompactedDB(t, opts)
	doTestDeleteAndWaitForCompaction(t, opts)
}

func doTestShouldReadCompactedDB(t *testing.T, options config.DBOptions) {
	t.Helper()
	bucket := objstore.NewInMemBucket()
	dbPath := "/tmp/test_kv_store"
	db, err := OpenWithOptions(context.Background(), dbPath, bucket, options)
	require.NoError(t, err)
	defer db.Close()

	manifestStore := store.NewManifestStore(dbPath, bucket)
	sm, err := store.LoadStoredManifest(manifestStore)
	require.NoError(t, err)
	storedManifest, ok := sm.Get()
	assert.True(t, ok)

	// write enough to fill up a few l0 SSTs
	for i := 0; i < 4; i++ {
		db.Put(repeatedChar(rune('a'+i), 32), bytes.Repeat([]byte{byte(1 + i)}, 32))
		db.Put(repeatedChar(rune('m'+i), 32), bytes.Repeat([]byte{byte(13 + i)}, 32))
	}
	waitForManifestCondition(storedManifest, time.Second*10, func(state *state.CoreStateSnapshot) bool {
		return state.L0LastCompacted.IsPresent() && len(state.L0) == 0
	})

	// write more l0s and wait for compaction
	for i := 0; i < 4; i++ {
		db.Put(repeatedChar(rune('f'+i), 32), bytes.Repeat([]byte{byte(6 + i)}, 32))
		db.Put(repeatedChar(rune('s'+i), 32), bytes.Repeat([]byte{byte(19 + i)}, 32))
	}
	waitForManifestCondition(storedManifest, time.Second*10, func(state *state.CoreStateSnapshot) bool {
		return state.L0LastCompacted.IsPresent() && len(state.L0) == 0
	})

	// write another l0
	db.Put(repeatedChar('a', 32), bytes.Repeat([]byte{128}, 32))
	db.Put(repeatedChar('m', 32), bytes.Repeat([]byte{129}, 32))

	val, err := db.Get(context.Background(), repeatedChar('a', 32))
	require.NoError(t, err)
	assert.Equal(t, bytes.Repeat([]byte{128}, 32), val)
	val, err = db.Get(context.Background(), repeatedChar('m', 32))
	require.NoError(t, err)
	assert.Equal(t, bytes.Repeat([]byte{129}, 32), val)

	for i := 1; i < 4; i++ {
		val, err := db.Get(context.Background(), repeatedChar(rune('a'+i), 32))
		require.NoError(t, err)
		assert.Equal(t, bytes.Repeat([]byte{byte(1 + i)}, 32), val)
		val, err = db.Get(context.Background(), repeatedChar(rune('m'+i), 32))
		require.NoError(t, err)
		assert.Equal(t, bytes.Repeat([]byte{byte(13 + i)}, 32), val)
	}

	for i := 0; i < 4; i++ {
		val, err := db.Get(context.Background(), repeatedChar(rune('f'+i), 32))
		require.NoError(t, err)
		assert.Equal(t, bytes.Repeat([]byte{byte(6 + i)}, 32), val)
		val, err = db.Get(context.Background(), repeatedChar(rune('s'+i), 32))
		require.NoError(t, err)
		assert.Equal(t, bytes.Repeat([]byte{byte(19 + i)}, 32), val)
	}

	_, err = db.Get(context.Background(), []byte("abc"))
	assert.ErrorIs(t, err, common.ErrKeyNotFound)
}

func doTestDeleteAndWaitForCompaction(t *testing.T, options config.DBOptions) {
	t.Helper()
	bucket := objstore.NewInMemBucket()
	dbPath := "/tmp/test_kv_store"
	db, err := OpenWithOptions(context.Background(), dbPath, bucket, options)
	require.NoError(t, err)
	defer db.Close()

	manifestStore := store.NewManifestStore(dbPath, bucket)
	sm, err := store.LoadStoredManifest(manifestStore)
	require.NoError(t, err)
	storedManifest, ok := sm.Get()
	assert.True(t, ok)

	// write enough to fill up a few l0 SSTs
	for i := 0; i < 4; i++ {
		db.Put(repeatedChar(rune('a'+i), 32), bytes.Repeat([]byte{byte(1 + i)}, 32))
		db.Put(repeatedChar(rune('m'+i), 32), bytes.Repeat([]byte{byte(13 + i)}, 32))
	}
	waitForManifestCondition(storedManifest, time.Second*10, func(state *state.CoreStateSnapshot) bool {
		return state.L0LastCompacted.IsPresent() && len(state.L0) == 0
	})

	// Delete existing keys
	for i := 0; i < 4; i++ {
		db.Delete(repeatedChar(rune('a'+i), 32))
		db.Delete(repeatedChar(rune('m'+i), 32))
	}
	//Add new keys
	for i := 0; i < 2; i++ {
		db.Put(repeatedChar(rune('f'+i), 32), bytes.Repeat([]byte{byte(6 + i)}, 32))
		db.Put(repeatedChar(rune('s'+i), 32), bytes.Repeat([]byte{byte(19 + i)}, 32))
	}
	waitForManifestCondition(storedManifest, time.Second*10, func(state *state.CoreStateSnapshot) bool {
		return state.L0LastCompacted.IsPresent() && len(state.L0) == 0
	})

	// verify that keys are deleted
	for i := 1; i < 4; i++ {
		_, err := db.Get(context.Background(), repeatedChar(rune('a'+i), 32))
		assert.ErrorIs(t, err, common.ErrKeyNotFound)
		_, err = db.Get(context.Background(), repeatedChar(rune('m'+i), 32))
		assert.ErrorIs(t, err, common.ErrKeyNotFound)
	}

	// verify that new keys added after deleting existing keys are present
	for i := 0; i < 2; i++ {
		val, err := db.Get(context.Background(), repeatedChar(rune('f'+i), 32))
		require.NoError(t, err)
		assert.Equal(t, bytes.Repeat([]byte{byte(6 + i)}, 32), val)
		val, err = db.Get(context.Background(), repeatedChar(rune('s'+i), 32))
		require.NoError(t, err)
		assert.Equal(t, bytes.Repeat([]byte{byte(19 + i)}, 32), val)
	}
}

func waitForManifestCondition(
	sm store.StoredManifest,
	timeout time.Duration,
	cond func(state *state.CoreStateSnapshot) bool,
) *state.CoreStateSnapshot {
	start := time.Now()
	for time.Since(start) < timeout {
		dbState, err := sm.Refresh()
		assert2.True(err == nil, "")
		if cond(dbState) {
			return dbState.Clone()
		}
		time.Sleep(time.Millisecond * 10)
	}
	panic("manifest condition took longer than timeout")
}

func testDBOptions(minFilterKeys uint32, l0SSTSizeBytes uint64) config.DBOptions {
	return config.DBOptions{
		FlushInterval:        100 * time.Millisecond,
		ManifestPollInterval: 100 * time.Millisecond,
		MinFilterKeys:        minFilterKeys,
		L0SSTSizeBytes:       l0SSTSizeBytes,
		CompressionCodec:     compress.CodecNone,
	}
}

func testDBOptionsCompactor(minFilterKeys uint32, l0SSTSizeBytes uint64, compactorOptions *config.CompactorOptions) config.DBOptions {
	dbOptions := testDBOptions(minFilterKeys, l0SSTSizeBytes)
	dbOptions.CompactorOptions = compactorOptions
	return dbOptions
}

func repeatedChar(ch rune, count int) []byte {
	return []byte(strings.Repeat(string(ch), count))
}
