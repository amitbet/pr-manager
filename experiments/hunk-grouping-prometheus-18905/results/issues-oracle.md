## oracle-003-0  [high] introduced=True unseen=True
**hasAppendIDAbove requires series lock but contract enforcement unclear**

- members: tsdb/head.go:(*Head).truncateStaleSeries, tsdb/head.go:(*Head).truncateSelectedSeries
- evidence: Function comment states "Must be called with s.Lock held" but the lock acquisition by iterForDeletion caller is not visible in the diff.
- scenario: If iterForDeletion does not lock the series before calling the predicate callback, concurrent writes could race with s.txs and s.txs.txIDCount access in hasAppendIDAbove, causing incorrect transaction iteration or data races.

## oracle-004-0  [medium] introduced=True unseen=True
**Data race on global testing callback variable**

- members: tsdb/db.go:type headViewFactory, tsdb/db.go:type headSeriesEvictor, tsdb/db.go:var compactHeadViewBeforeEvictTestingCallback
- evidence: Declaration: `var compactHeadViewBeforeEvictTestingCallback func()` (line 1569). Usage in compactHeadViewLocked: `if compactHeadViewBeforeEvictTestingCallback != nil { compactHeadViewBeforeEvictTestingCallback(); compactHeadViewBeforeEvictTestingCallback = nil }`
- scenario: If test harness runs tests in parallel (e.g., go test -parallel), different DB instances can call compactHeadViewLocked concurrently, both accessing and modifying the global callback variable. One test's cleanup could set it to nil while another test's compaction reads/calls it, causing nil dereference panic or skipped/wrong callback execution. Also vulnerable if cleanup runs asynchronously.

## oracle-007-0  [high] introduced=True unseen=True
**Meta().MaxTime conversion may disagree with compactor Write API**

- members: tsdb/db.go:(*DB).CompactStaleHead
- evidence: uids, err := db.compactor.Write(db.dir, view, view.Meta().MinTime, view.Meta().MaxTime+1, meta)
- scenario: If compactor.Write expects an inclusive max time (not exclusive), passing MaxTime+1 causes block time ranges to be off by one, corrupting block metadata. Subsequent queries may skip or double-count samples at range boundaries.

## oracle-007-1  [high] introduced=True unseen=False
**AppendIDWatermark captured before isolation snapshot may miss concurrent writes**

- members: tsdb/db.go:(*DB).CompactStaleHead
- evidence: appendIDWatermark := db.head.iso.lastAppendID() // captured before loop
for ; mint <= maxt; mint += ... {
    uids, err := db.compactor.Write(...) // block write happens here
    if err := db.reloadBlocks() { ... }
}
if err := evict(maxt, appendIDWatermark)
- scenario: A sample appended to series S after watermark is captured but before its block is written will be included in the block. When evict() checks hasAppendIDAbove(S, watermark) later, it skips S because the appendID is > watermark. But the sample IS in the block, then S gets deleted from head anyway during a later compaction cycle, orphaning that sample in a block that still claims to contain S.

## oracle-007-2  [medium] introduced=True unseen=True
**hasAppendIDAbove loop iterates s.txs.txIDCount but never advances iterator past boundary**

- members: tsdb/db.go:(*DB).CompactStaleHead
- evidence: it := s.txs.iterator()
for i := uint32(0); i < s.txs.txIDCount; i++ {
    if it.At() > watermark {
        return true
    }
    it.Next()
}
return false
- scenario: If txIDCount changes concurrently (txs being modified), the loop may iterate past the valid range or miss entries. Iterator bounds are not checked; if txIDCount > actual iterator size, it.At() may read invalid data.

## oracle-014-0  [low] introduced=True unseen=False
**Missing explicit DB cleanup in test**

- members: tsdb/db_test.go:TestCompactSelectedSeries_SparseSelectedAcrossWideHead, tsdb/db_test.go:TestCompactSelectedSeries_EvictedSeriesRecordKeptInCheckpoint
- evidence: TestCompactSelectedSeries_SparseSelectedAcrossWideHead and TestCompactSelectedSeries_EvictedSeriesRecordKeptInCheckpoint do not call db.Close() via t.Cleanup()
- scenario: If test framework does not automatically clean up resources, the test may leave DB handles and temp directories open, causing resource leaks. Other tests in the diff use t.Cleanup(func() { require.NoError(t, db.Close()) }).

## oracle-014-1  [low] introduced=True unseen=True
**Hard-coded WAL checkpoint iteration limit may cause flaky test**

- members: tsdb/db_test.go:TestCompactSelectedSeries_SparseSelectedAcrossWideHead, tsdb/db_test.go:TestCompactSelectedSeries_EvictedSeriesRecordKeptInCheckpoint
- evidence: TestCompactSelectedSeries_EvictedSeriesRecordKeptInCheckpoint loops at most 10 times before expecting LastCheckpoint to succeed at line 10339
- scenario: If the WAL implementation requires more than 10 truncation cycles to produce a checkpoint (e.g., under high load or with certain WAL configuration), the test fails with 'a checkpoint must have been produced' even though the feature works correctly.

## oracle-016-0  [medium] introduced=True unseen=False
**BenchmarkCompactSelectedSeries restarts timer after cleanup**

- members: tsdb/compact_test.go:BenchmarkCompactSelectedSeries, tsdb/compact_test.go:BenchmarkFilterSeriesAndSortPostings
- evidence: Line 1415-1416: `b.StopTimer()` after `db.Close()` followed by `b.StartTimer()`
- scenario: Timer is restarted with no measured work remaining in the loop body. The timer runs through the b.Loop() condition check and loop overhead before the next iteration's b.StopTimer() is called, causing benchmark measurements to include inter-iteration overhead and producing inaccurate performance data.
