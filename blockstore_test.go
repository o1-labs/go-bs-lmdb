package lmdbbs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	ipfsUtil "github.com/ipfs/go-ipfs-util"
	logger "github.com/ipfs/go-log/v2"
	"github.com/multiformats/go-multihash"
	bstest "github.com/raulk/go-bs-tests"
	"github.com/stretchr/testify/require"
)

func init() {
	logger.SetupLogging(logger.Config{Stdout: true})
}

func TestLMDBBlockstore(t *testing.T) {
	sync := Options{NoSync: false}
	s := &bstest.Suite{
		NewBlockstore:  newBlockstore(sync),
		OpenBlockstore: openBlockstore(sync),
	}
	s.RunTests(t, "sync")

	nosync := Options{NoSync: true}
	s = &bstest.Suite{
		NewBlockstore:  newBlockstore(nosync),
		OpenBlockstore: openBlockstore(nosync),
	}
	s.RunTests(t, "nosync")
}

func newBlockstore(opts Options) func(tb testing.TB) (bstest.Blockstore, string) {
	return func(tb testing.TB) (bstest.Blockstore, string) {
		tb.Helper()

		path, err := ioutil.TempDir("", "")
		if err != nil {
			tb.Fatal(err)
		}

		opts.Path = path
		db, err := Open(&opts)
		if err != nil {
			tb.Fatal(err)
		}

		tb.Cleanup(func() {
			_ = os.RemoveAll(path)
		})

		return db, path
	}
}

func openBlockstore(opts Options) func(tb testing.TB, path string) (bstest.Blockstore, error) {
	return func(tb testing.TB, path string) (bstest.Blockstore, error) {
		opts.Path = path
		return Open(&opts)
	}
}

func checkBlocksBs(t *testing.T, bs blockstore.Viewer, blocks []blocks.Block) {
	for _, b := range blocks {
		require.NoError(t, bs.View(b.Cid(), func(data []byte) error {
			if !bytes.Equal(data, b.RawData()) {
				return fmt.Errorf("unexpected value for a key")
			}
			return nil
		}))
	}
}

func TestMmapExpansionAndParallelContext(t *testing.T) {
	opts := Options{InitialMmapSize: 1 << 20, NoLock: true} // 1MiB.

	bs, path := newBlockstore(opts)(t)

	info, err := bs.(*Blockstore).env.Info()
	require.NoError(t, err)
	prev := info.MapSize

	blocks1 := putEntries(t, bs, 16*1024, 1*1024)

	info, err = bs.(*Blockstore).env.Info()
	require.NoError(t, err)
	current := info.MapSize
	require.Greater(t, current, prev)

	opts.ReadOnly = true
	// reopen the database with the original initial mmap size.
	bs2, err := openBlockstore(opts)(t, path)
	require.NoError(t, err)

	info, err = bs2.(*Blockstore).env.Info()
	require.NoError(t, err)
	reopened := info.MapSize
	require.Greater(t, int(reopened), 1<<23)

	checkBlocksBs(t, bs2, blocks1)

	// verify that we can add more entries, and that we grow again.
	blocks2 := putEntries(t, bs, 16*1024, 1*1024)
	info, err = bs.(*Blockstore).env.Info()
	require.NoError(t, err)
	final := info.MapSize
	require.Greater(t, final, reopened)

	checkBlocksBs(t, bs2, blocks2)
}
func TestMmapExpansionSucceedsReopen(t *testing.T) {
	opts := Options{InitialMmapSize: 1 << 20} // 1MiB.

	bs, path := newBlockstore(opts)(t)

	info, err := bs.(*Blockstore).env.Info()
	require.NoError(t, err)
	prev := info.MapSize

	blocks1 := putEntries(t, bs, 16*1024, 1*1024)

	info, err = bs.(*Blockstore).env.Info()
	require.NoError(t, err)
	current := info.MapSize
	require.Greater(t, current, prev)

	// close the db.
	require.NoError(t, bs.(io.Closer).Close())

	// reopen the database with the original initial mmap size.
	bs, err = openBlockstore(opts)(t, path)
	require.NoError(t, err)

	info, err = bs.(*Blockstore).env.Info()
	require.NoError(t, err)
	reopened := info.MapSize
	require.Greater(t, int(reopened), 1<<23)

	checkBlocksBs(t, bs, blocks1)

	// verify that we can add more entries, and that we grow again.
	putEntries(t, bs, 16*1024, 1*1024)
	info, err = bs.(*Blockstore).env.Info()
	require.NoError(t, err)
	final := info.MapSize
	require.Greater(t, final, reopened)
}

func TestNoMmapExpansion(t *testing.T) {
	opts := Options{InitialMmapSize: 64 << 20} // 64MiB, a large enough mmap size.

	bs, _ := newBlockstore(opts)(t)
	defer bs.(io.Closer).Close()

	info, err := bs.(*Blockstore).env.Info()
	require.NoError(t, err)
	prev := info.MapSize

	putEntries(t, bs, 16*1024, 1*1024)

	info, err = bs.(*Blockstore).env.Info()
	require.NoError(t, err)
	current := info.MapSize
	require.EqualValues(t, prev, current)
}

func TestMmapExpansionWithCursors(t *testing.T) {
	opts := Options{InitialMmapSize: 64 << 20} // 64MiB, a large enough mmap size.

	bs, _ := newBlockstore(opts)(t)
	defer bs.(io.Closer).Close()

	putEntries(t, bs, 1*1024, 1*1024)

	// cursor 1.
	ctx, cancel := context.WithCancel(context.Background())
	ch1, err := bs.AllKeysChan(ctx)
	require.NoError(t, err)
	<-ch1 // consume one entry

	// cursor 2.
	ch2, err := bs.AllKeysChan(ctx)
	require.NoError(t, err)
	<-ch2 // consume one entry

	// cursor 3.
	ch3, err := bs.AllKeysChan(ctx)
	require.NoError(t, err)
	<-ch3 // consume one entry

	// add more entries to force the mmap to grow.
	putEntries(t, bs, 4*1024, 1*1024)

	var i int
	for range ch1 {
		i++ // verify that the cursor continues running and eventually finishes.
	}
	require.Greater(t, i, 1*1024) // we see entries from the second insertion batch.

	i = 0
	for range ch2 {
		i++ // verify that the cursor continues running and eventually finishes.
	}
	require.Greater(t, i, 1*1024) // we see entries from the second insertion batch.

	i = 0
	for range ch3 {
		i++ // verify that the cursor continues running and eventually finishes.
	}
	require.Greater(t, i, 1*1024) // we see entries from the second insertion batch.

	cancel()
}

func TestGrowUnderConcurrency(t *testing.T) {
	opts := Options{ // set a really aggressive policy that makes the mmap grow very frequently.
		InitialMmapSize:      1 << 10,
		MmapGrowthStepFactor: 1.5,
		MmapGrowthStepMax:    2 << 10,
	}

	bs, _ := newBlockstore(opts)(t)
	defer bs.(io.Closer).Close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ { // 20 writers.
		wg.Add(1)
		go func() {
			defer wg.Done()
			putEntries(t, bs, 1*1024, 1*1024)
		}()
	}

	for i := 0; i < 20; i++ { // 20 queriers for random CIDs.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1024; i++ {
				_, _ = bs.Get(randomCID())
			}
		}()
	}

	for i := 0; i < 20; i++ { // 20 deleters of random CIDs.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1024; i++ {
				_ = bs.DeleteBlock(randomCID())
			}
		}()
	}

	for i := 0; i < 20; i++ { // 20 cursors.
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, _ := bs.AllKeysChan(context.Background())
			for range ch {
			}
		}()
	}

	wg.Wait()
}

func TestRetryWhenReadersFull(t *testing.T) {
	opts := Options{
		MaxReaders: 1, // single reader to induce contention.
	}

	bs, _ := newBlockstore(opts)(t)
	defer bs.(io.Closer).Close()

	blocks := putEntries(t, bs, 1*1024, 1*1024)

	// this context cancels in 2 seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := bs.AllKeysChan(ctx)
	require.NoError(t, err)
	k1 := <-ch // consume one element, then leave it hanging.
	keyExists := false
	for _, block := range blocks {
		keyExists = keyExists || k1.Equals(block.Cid())
	}
	require.True(t, keyExists, "Key received from the channel is unknown")

	// this get will block until the cursor has finished.
	start := time.Now()
	_, err = bs.Get(randomCID())
	require.Equal(t, blockstore.ErrNotFound, err)
	require.GreaterOrEqual(t, time.Since(start).Nanoseconds(), 1*time.Second.Nanoseconds())
}

// TestMmapExpansionPutMany tests that a PutMany operation yields when it
// encounters a MDB_MAP_FULL error, and that it retries once the grow finishes.
func TestMmapExpansionPutMany(t *testing.T) {
	opts := Options{
		InitialMmapSize:      1 << 10,
		MmapGrowthStepFactor: 1.5,
		MmapGrowthStepMax:    2 << 10,
	}

	bs, _ := newBlockstore(opts)(t)
	defer bs.(io.Closer).Close()

	var blks []blocks.Block
	for i := 0; i < 1024; i++ {
		b := make([]byte, 1024)
		rand.Read(b)
		c := cid.NewCidV1(cid.Raw, ipfsUtil.Hash(b)) // makes a copy of k
		blk, err := blocks.NewBlockWithCid(b, c)
		require.NoError(t, err)
		blks = append(blks, blk)
	}

	err := bs.PutMany(blks)
	require.NoError(t, err)
}

func putEntries(t *testing.T, bs bstest.Blockstore, count int, size int) []blocks.Block {
	res := make([]blocks.Block, 0, count)
	for i := 0; i < count; i++ {
		b := make([]byte, size)
		rand.Read(b)
		c := cid.NewCidV1(cid.Raw, ipfsUtil.Hash(b)) // makes a copy of k
		blk, err := blocks.NewBlockWithCid(b, c)
		require.NoError(t, err)
		require.NoError(t, bs.Put(blk))
		res = append(res, blk)
	}
	return res
}

func randomCID() cid.Cid {
	b := make([]byte, 32)
	rand.Read(b)
	mh, _ := multihash.Encode(b, multihash.SHA2_256)
	return cid.NewCidV1(cid.Raw, mh)
}

type deleteManyer interface {
	DeleteMany([]cid.Cid) error
}

func TestDeleteMany(t *testing.T) {
	opts := Options{
		InitialMmapSize:      1 << 10,
		MmapGrowthStepFactor: 1.5,
		MmapGrowthStepMax:    2 << 10,
	}

	bs, _ := newBlockstore(opts)(t)
	defer bs.(io.Closer).Close()

	blocks := putEntries(t, bs, 500, 1024)
	todelete := make([]cid.Cid, 100)
	for i := range todelete {
		todelete[i] = blocks[i].Cid()
	}

	require.NoError(t, bs.DeleteBlock(todelete[5]))
	require.NoError(t, bs.DeleteBlock(todelete[5]))

	dm := bs.(deleteManyer)

	if err := dm.DeleteMany(todelete); err != nil {
		t.Fatal(err)
	}

	deleted := make(map[cid.Cid]bool)
	for _, d := range todelete {
		deleted[d] = true
	}

	ch, err := bs.AllKeysChan(context.TODO())
	if err != nil {
		t.Fatal(err)
	}

	for c := range ch {
		if deleted[c] {
			t.Fatal("found cid in blockstore we deleted")
		}
	}
}
