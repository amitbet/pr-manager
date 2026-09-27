## linked-004-0  [low] introduced=True unseen=True
**Race condition on global testing callback variable**

- members: tsdb/db.go:type headViewFactory, tsdb/db.go:type headSeriesEvictor, tsdb/db.go:var compactHeadViewBeforeEvictTestingCallback, tsdb/db.go:(*DB).compactHeadViewLocked
- evidence: Line ~1776-1779: `if compactHeadViewBeforeEvictTestingCallback != nil { compactHeadViewBeforeEvictTestingCallback(); ... }` reads and calls a global variable that is written by tests (line in test: `compactHeadViewBeforeEvictTestingCallback = func() { ... }`)
- scenario: If test sets the callback while another test or production code concurrently calls compactHeadViewLocked (e.g., in parallel test execution or multiple goroutines), a data race occurs on reading/writing the function pointer. Go's race detector would flag this as `concurrent map/slice read and write`.

## linked-006-0  [low] introduced=True unseen=False
**Stale comment references old type in SortedPostings.**

- members: tsdb/head_read.go:type headSelectedSeriesIndexReader, tsdb/head_read.go:type allSelectedSeriesPostings, tsdb/head_read.go:(*Head).selectedSeriesIndex, tsdb/head_read.go:(*headSelectedSeriesIndexReader).Postings, tsdb/head_read.go:(*headSelectedSeriesIndexReader).SortedPostings, tsdb/head_read.go:(*headSelectedSeriesIndexReader).PostingsForLabelMatching, tsdb/head_read.go:(*headSelectedSeriesIndexReader).PostingsForAllLabelValues
- evidence: "This implementation expects the input postings to be the one returned by headStaleIndexReader.Postings() with AllPostingsKey"
- scenario: Comment on the new headSelectedSeriesIndexReader.SortedPostings method still mentions headStaleIndexReader.Postings(), confusing readers about which method is being described.

## linked-007-0  [medium] introduced=True unseen=False
**Duration metric not observed on error returns**

- members: tsdb/db.go:(*DB).CompactSelectedSeries
- evidence: Lines with `if err != nil { return err }` after filterSeriesAndSortPostings (~1850) and compactHeadViewLocked (~1886) return early before reaching the metric observation at the end; contrast with the all-filtered-out case which explicitly observes at line ~1862
- scenario: When filterSeriesAndSortPostings or compactHeadViewLocked fail and the function returns an error, selectedSeriesCompactionDuration histogram is not populated. Successful compactions and no-op returns (returning nil) both record durations, but failures don't, leaving gaps in timing data that operators depend on for monitoring and diagnostics.

## linked-012-0  [medium] introduced=True unseen=False
**Missing database cleanup in boundary sample test**

- members: tsdb/db_test.go:TestCompactSelectedSeries_MultipleChunkRanges, tsdb/db_test.go:TestCompactSelectedSeries_ChunkBoundarySampleNotLost
- evidence: TestCompactSelectedSeries_ChunkBoundarySampleNotLost creates db via `newTestDB(t, withOpts(opts))` at line 10166 without calling `t.Cleanup(func() { require.NoError(t, db.Close()) })`
- scenario: Database handle remains open after test completes; resource accumulation over multiple test runs; potential file descriptor exhaustion in CI.

## linked-016-0  [low] introduced=True unseen=True
**Missing database cleanup in TestCompactSelectedSeries_SparseSelectedAcrossWideHead**

- members: tsdb/db_test.go:TestCompactSelectedSeries_SparseSelectedAcrossWideHead, tsdb/db_test.go:TestCompactSelectedSeries_EvictedSeriesRecordKeptInCheckpoint
- evidence: Function ends without `t.Cleanup(func() { require.NoError(t, db.Close()) })`
- scenario: Database created by newTestDB(t, ...) is never closed. Resources, WAL segments, and temporary block files remain allocated after test ends.

## linked-016-1  [low] introduced=True unseen=True
**Missing database cleanup in TestCompactSelectedSeries_EvictedSeriesRecordKeptInCheckpoint**

- members: tsdb/db_test.go:TestCompactSelectedSeries_SparseSelectedAcrossWideHead, tsdb/db_test.go:TestCompactSelectedSeries_EvictedSeriesRecordKeptInCheckpoint
- evidence: Function ends without `t.Cleanup(func() { require.NoError(t, db.Close()) })`
- scenario: Database created by newTestDB(t, ...) is never closed. Resources, WAL segments, and checkpoint files remain allocated after test ends.

## linked-018-0  [low] introduced=True unseen=False
**Database not closed on CompactSelectedSeries error**

- members: tsdb/compact_test.go:BenchmarkCompactSelectedSeries, tsdb/compact_test.go:BenchmarkFilterSeriesAndSortPostings
- evidence: `require.NoError(b, db.CompactSelectedSeries(selectedRefs))` followed by `b.StopTimer()` then `require.NoError(b, db.Close())` on lines ~1410-1413
- scenario: CompactSelectedSeries returns error → require.NoError panics with Fatalf → db.Close() never executes → database file handles remain open until b.TempDir() cleanup
