## baseline-000-0  [low] introduced=True unseen=True
**Data race on package-global testing callback variable**

- members: tsdb/db.go:(*DB).compactHeadViewLocked
- evidence: if compactHeadViewBeforeEvictTestingCallback != nil { ... compactHeadViewBeforeEvictTestingCallback = nil }
- scenario: If multiple DB instances perform concurrent compactions and both reach this check point with the callback non-nil, both can read it as non-nil before either sets it to nil. The callback could be called twice instead of once, causing test assertions to fail or side effects to occur unexpectedly.

## baseline-007-0  [high] introduced=True unseen=True
**Series lock requirement not visibly enforced**

- members: tsdb/head.go:hasAppendIDAbove
- evidence: Docstring states "Must be called with s.Lock held", but the call path is: stripeSeries.gcSeries → s.iterForDeletion(check) → check(...) → shouldEvict(series) → hasAppendIDAbove(s, watermark). No visible series.Lock acquisition before hasAppendIDAbove.
- scenario: If iterForDeletion doesn't acquire series.Lock before invoking the predicate, concurrent appends could modify s.txs during iteration, corrupting the iterator state or causing the watermark check to return false when it should return true. The series would then be evicted despite containing samples with appendID > watermark, causing data loss.

## baseline-018-0  [medium] introduced=True unseen=True
**NumSeries() does not filter like Index() does**

- members: tsdb/head.go:(*SelectedSeriesHead).NumSeries
- evidence: `return uint64(len(h.selectedSeriesRefs.sortedByRef))` returns raw count without filtering
- scenario: SelectedSeriesHead with refs [ref1, ref2] where ref2 has OOO data: NumSeries() returns 2, but selectedSeriesIndex filters ref2, so visible series count is 1. BlockMeta.Stats.NumSeries would be 2 while actual block contains 1 series.

## baseline-028-0  [low] introduced=True unseen=False
**Division by zero when count is 0**

- members: tsdb/compact_test.go:pickRefsEvenly
- evidence: step := float64(len(refs)) / float64(count)
- scenario: Calling pickRefsEvenly(refs, 0) with non-empty refs causes runtime panic: division by zero

## baseline-029-0  [medium] introduced=True unseen=False
**Database not closed if CompactSelectedSeries fails**

- members: tsdb/compact_test.go:BenchmarkCompactSelectedSeries
- evidence: require.NoError(b, db.CompactSelectedSeries(selectedRefs)) at line 1345 followed by b.StopTimer() and require.NoError(b, db.Close()) at lines 1347-1348
- scenario: If CompactSelectedSeries returns an error, require.NoError calls b.Fatal() which panics and prevents db.Close() from executing. Database resources (file descriptors, memory) are leaked within the benchmark iteration.

## baseline-032-0  [medium] introduced=True unseen=False
**Data race on global testing callback variable**

- members: tsdb/db.go:var compactHeadViewBeforeEvictTestingCallback
- evidence: `var compactHeadViewBeforeEvictTestingCallback func()` at package scope; both tests set it and production code (called from tests) reads/writes it without synchronization. Test sets it: `compactHeadViewBeforeEvictTestingCallback = func() { ... }`, invoked in `compactHeadViewLocked`: `if compactHeadViewBeforeEvictTestingCallback != nil { compactHeadViewBeforeEvictTestingCallback(); compactHeadViewBeforeEvictTestingCallback = nil }`
- scenario: Two tests running concurrently both set different callbacks to this global; Test A's compaction may read and execute Test B's callback instead, causing wrong test behavior or failure.

## baseline-044-0  [low] introduced=True unseen=True
**Initial DB not cleaned up if test fails before restart**

- members: tsdb/db_test.go:TestCompactSelectedSeries_LateAppendDuringCompactionSurvivesRestart
- evidence: Lines 10062-10063 create DB1 without cleanup registration. Only the callback cleanup (line 10102) is registered before line 10137 where DB1 is explicitly closed. The DB2 cleanup (line 10139) only applies after reassignment.
- scenario: Test assertion fails at line 10105 (hookErr check) or any subsequent line before db.Close() at line 10137. DB1 remains open and is not cleaned up at end of test.

## baseline-046-0  [low] introduced=True unseen=False
**Test missing database cleanup**

- members: tsdb/db_test.go:TestCompactSelectedSeries_SparseSelectedAcrossWideHead
- evidence: Test function ends without `t.Cleanup(func() { require.NoError(t, db.Close()) })`
- scenario: Test completes without closing the database, leaking resources (file handles, memory). If run in parallel with other tests, may interfere with them or cause resource exhaustion.
