## deterministic-005-0  [high] introduced=True unseen=False
**Nil pointer when isolation is disabled in compactHeadViewLocked**

- members: tsdb/db.go:type dbMetrics
- evidence: appendIDWatermark := db.head.iso.lastAppendID() at line ~1745
- scenario: Call CompactSelectedSeries or CompactStaleHead with IsolationDisabled=true → db.head.iso is nil → panic on .lastAppendID()

## deterministic-009-0  [low] introduced=True unseen=True
**Initial DB not cleaned up on early assertion failure**

- members: tsdb/db_test.go:TestCompactSelectedSeries_LateAppendDuringCompactionSurvivesRestart
- evidence: `db := newTestDB(t, withOpts(opts))` at line ~10064 is never registered with `t.Cleanup()`, only the DB reopened at line ~10146 is. The initial DB is only closed explicitly at line ~10145.
- scenario: If any `require.NoError()` or other `require.` assertion before line ~10145 fails, calling `t.FailNow()` and exiting the test function early, the original DB remains unclosed and is leaked. This can happen at preconditions like the `require.LessOrEqual(t, appendableMinValid, int64(400), ...)` check at line ~10082.
