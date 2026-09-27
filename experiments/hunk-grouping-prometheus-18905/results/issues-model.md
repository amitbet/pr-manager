## model-000-0  [low] introduced=True unseen=False
**SortedPostings comment references old type name**

- members: tsdb/head_read.go:type seriesRefs, tsdb/head_read.go:type headSelectedSeriesIndexReader, tsdb/head_read.go:type allSelectedSeriesPostings, tsdb/head.go:type SelectedSeriesHead, tsdb/head.go:var selectedSeriesHeadULID, tsdb/head.go:isSeriesWithoutOOO, tsdb/head.go:isStaleSeries, tsdb/head.go:hasAppendIDAbove, tsdb/block.go:const indexFilename
- evidence: Comment line: "This implementation expects the input postings to be the one returned by headStaleIndexReader.Postings()"
- scenario: Developer reading the method documentation sees reference to 'headStaleIndexReader' but the function is actually on 'headSelectedSeriesIndexReader', causing confusion about which type's Postings() returns the expected marker.

## model-002-0  [low] introduced=True unseen=False
**Comment references old type name in SortedPostings**

- members: tsdb/head_read.go:(*Head).selectedSeriesIndex, tsdb/head_read.go:(*headSelectedSeriesIndexReader).Postings, tsdb/head_read.go:(*headSelectedSeriesIndexReader).SortedPostings, tsdb/head_read.go:(*headSelectedSeriesIndexReader).PostingsForLabelMatching, tsdb/head_read.go:(*headSelectedSeriesIndexReader).PostingsForAllLabelValues
- evidence: // This implementation expects the input postings to be the one returned by headStaleIndexReader.Postings() with AllPostingsKey,
- scenario: Developer reading the SortedPostings method sees a comment mentioning the old headStaleIndexReader class, creating confusion about which type is actually involved. The comment should refer to headSelectedSeriesIndexReader instead.

## model-004-0  [high] introduced=True unseen=True
**Implicit lock requirement for `shouldEvict` callback**

- members: tsdb/head_read.go:(*Head).filterSeriesAndSortPostings, tsdb/head_read.go:(*Head).staleSeriesRefsNoOOOData, tsdb/head.go:(*stripeSeries).gcSeries, tsdb/head.go:(*Head).gcSeries
- evidence: `hasAppendIDAbove(s, appendIDWatermark)` called from `shouldEvict` callback in `truncateSeries` → `gcSeries` → `iterForDeletion`, but lock not visibly held
- scenario: If `iterForDeletion` does not hold the series lock when invoking the check callback, `hasAppendIDAbove` races when reading `s.txs` and `s.txs.txIDCount`, potentially skipping series that should be evicted or evicting series with in-flight appends that may not have made it to the compacted block.

## model-007-0  [medium] introduced=True unseen=False
**Triggered counter may not reflect actual compaction attempts**

- members: tsdb/db.go:(*DB).CompactSelectedSeries
- evidence: Line 1841 (`db.metrics.selectedSeriesCompactionsTriggered.Inc()`) occurs before line 1854–1859 (early return if `len(selectedSeriesRefs.sortedByRef) == 0`)
- scenario: Caller invokes `CompactSelectedSeries([]storage.SeriesRef{ref1, ref2, ...})` where all refs have out-of-order data. Counter increments to indicate a trigger, but no blocks are written and no series are evicted because all refs are filtered out. Test coverage does not verify this edge case.

## model-012-0  [medium] introduced=True unseen=False
**Unnecessary StartTimer at loop end skews measurements**

- members: tsdb/compact_test.go:BenchmarkCompactSelectedSeries, tsdb/compact_test.go:BenchmarkFilterSeriesAndSortPostings
- evidence: Line 1398: `b.StartTimer()` after `db.Close()` inside the `for b.Loop()` loop
- scenario: Timer left running after cleanup and before next iteration's StopTimer call. Timer will be active during setup of subsequent iteration (lines 1389-1391), including that setup time in measurements and skewing benchmark results.

## model-017-0  [medium] introduced=True unseen=True
**Test sensitive to hook invocation point not verified**

- members: tsdb/db_test.go:TestCompactSelectedSeries_LateAppendDuringCompactionSurvivesRestart
- evidence: The callback `compactHeadViewBeforeEvictTestingCallback` is set and cleaned up, but its invocation in `compactHeadViewLocked` is not shown. When `defaultIsolationDisabled=true`, the test would pass even if the hook is never called.
- scenario: If `compactHeadViewLocked` fails to call the hook, the late sample is never appended. In isolation-disabled mode, the test still expects 2 samples and would get 2 (from the block), passing silently without testing the intended race condition.

## model-017-1  [low] introduced=True unseen=False
**Global callback variable without synchronization**

- members: tsdb/db_test.go:TestCompactSelectedSeries_LateAppendDuringCompactionSurvivesRestart
- evidence: Line sets global `compactHeadViewBeforeEvictTestingCallback` and cleans up via `t.Cleanup()`. Multiple concurrent uses of this variable are not protected.
- scenario: If tests run in parallel or cleanup is delayed, test interference could occur. One test's hook could be invoked by another test's compaction, or vice versa.

## model-018-0  [medium] introduced=True unseen=False
**Missing database cleanup in test**

- members: tsdb/db_test.go:TestCompactSelectedSeries_SparseSelectedAcrossWideHead, tsdb/db_test.go:TestCompactSelectedSeries_EvictedSeriesRecordKeptInCheckpoint
- evidence: db := newTestDB(t, withOpts(opts))
db.DisableCompactions()
- scenario: Test completes without releasing database resources; file descriptors remain open, causing cumulative resource depletion across test runs and potential 'too many open files' errors.

## model-018-1  [medium] introduced=True unseen=False
**Missing database cleanup in test**

- members: tsdb/db_test.go:TestCompactSelectedSeries_SparseSelectedAcrossWideHead, tsdb/db_test.go:TestCompactSelectedSeries_EvictedSeriesRecordKeptInCheckpoint
- evidence: db := newTestDB(t, withOpts(opts))
db.DisableCompactions()
- scenario: Test completes without releasing database resources; file descriptors remain open, causing cumulative resource depletion across test runs and potential 'too many open files' errors.
