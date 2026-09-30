# PR 01: Read-only hot open with events, and `catalog.OpenReadOnly`

| | |
|---|---|
| Branch | `bench-campaign-v2/01-readonly-open` |
| Repository | marwen-abid/stellar-rpc (base `feature/full-history` at `91f158b` or later) |
| Depends on | None. D5 is confirmed. |
| Implements | D5; spec Section 6.2 (not the whole-root test, which is PR 05) |
| Estimate | About 120 non-test lines: `stores/hotchunk/hotchunk.go` 40, `catalog/catalog.go` 30, `catalog/secret.go` 10, `query/resolve.go` and `query/registry.go` comments 6, `rpcv2test/fileset/fileset.go` 35 |

`P` is `cmd/stellar-rpc/internal/rpcv2`. All identifiers below were read at `91f158b`.

## 1. Goal

After this PR, a hot chunk database can be opened read-only with a working events facade, so hot `getEvents` works on a read-only handle. A catalog can be opened read-only. It fails when the catalog does not exist and it writes no file. A test proves that open, reads and close leave the file set of a hot chunk directory and of a catalog directory unchanged.

## 2. Scope

- Add `hotchunk.OpenReadOnlyWithEvents` and extend the internal `open` in `P/stores/hotchunk/hotchunk.go`.
- Add `catalog.OpenReadOnly` in `P/catalog/catalog.go`, with a load-only secret read in `P/catalog/secret.go`.
- Rewrite the comments that say a read-only open is ledgers-only: `P/stores/hotchunk/hotchunk.go`, `P/query/resolve.go` (`ReadView.Events` doc), `P/query/registry.go` (`publishReadyHandles` doc).
- Add a test-only file-set helper package `P/rpcv2test/fileset`.
- Add the file-set tests for a hot chunk directory and a catalog directory.

## 3. Out of scope (boundaries)

- The file-set test over the whole dataset root while `bench-serve` opens, serves and closes: PR 05.
- Use of the new opens in the read path (`query.NewRegistry`, `PublishHandle`): PR 05. This PR changes no caller.
- `OpenReadOnly` behaviour. It stays ledgers-only for the freeze source and the startup refiner (#834). `TestOpenReadOnly_SkipsEventsWarmup` stays green.
- A ready-state gate on the new open (as `openReady` does). `bench-serve` gets its chunks from `Catalog.ReadyHotChunkKeys`, so the gate adds nothing. Decide again in PR 05 if needed.
- A LOCK or any check against a concurrent writer. RocksDB documents undefined behaviour for a read-only open beside a writer (facts A1). The spec rule "never open a dataset read-only while a writer has it open" stays a usage rule.

## 4. Design

### 4.1 Hot chunk open

Current code (`hotchunk.go`):

- `open(path string, chunkID chunk.ID, logger *supportlog.Entry, readOnly, mustExist bool) (*DB, error)` skips `event.NewWithStore` when `readOnly` is true (`if readOnly { return db, nil }`).
- `OpenReadOnly` calls `open(path, chunkID, logger, true, false)`.
- `(*DB).Events` panics when `d.events == nil`.
- `(*DB).IngestLedger` uses `d.events == nil` as its read-only guard.

Facts: the events warmup (`warmup`, `warmupIndex`, `warmupOffsets`, `verifyChunkConsistency` in `P/stores/event/hot_store.go`) calls only `Store.Iterate`, `Store.IterateRange` and `Store.Get`. A probe ran `event.NewWithStore` over a read-only `rocksdb.Store` and it worked (facts section 1, D5-design). A read-only open plus reads plus close changed no name, size or mtime in a hot chunk directory (facts S1).

Change:

1. Replace the two booleans of `open` with one value, so that four call sites read clearly:

   ```go
   type openMode struct{ readOnly, mustExist, events bool }
   func open(path string, chunkID chunk.ID, logger *supportlog.Entry, m openMode) (*DB, error)
   ```

   `Open` = `{events: true}`. `OpenExisting` = `{mustExist: true, events: true}`. `OpenReadOnly` = `{readOnly: true}`. The new function = `{readOnly: true, events: true}`. `config(path, logger, readOnly, mustExist)` does not change (tests call it).
2. Add:

   ```go
   // OpenReadOnlyWithEvents opens an existing hot DB read-only and composes all three facades.
   func OpenReadOnlyWithEvents(path string, chunkID chunk.ID, logger *supportlog.Entry) (*DB, error)
   ```

   The open runs the events warmup once. It costs one scan of the events index CF and the offsets CF per chunk. Callers that read only ledgers keep `OpenReadOnly`.
3. Add a field `readOnly bool` to `DB`. `IngestLedger` rejects a write when `d.readOnly` is true, with the current error text. Keep the `d.events == nil` check out of this guard. Reason: with the new open, a read-only DB has events, so the old guard would let the call reach `Store.Batch`. RocksDB rejects the batch, but the error must come before the batch callback.
4. `Events()` keeps its panic for a DB without events. Rewrite its doc: the panic applies to `OpenReadOnly` handles only.

Name: `OpenReadOnlyWithEvents` is the choice. It names the one difference from `OpenReadOnly`. Rejected: `OpenServeView` (names a caller, which Section 11 rule 2 forbids).

### 4.2 Catalog read-only open

Current code (`catalog.go`, `Open`): `rocksdb.New(rocksdb.Config{Path: path, Logger: logger})` (read-write, create-if-missing), then `census()`, then `ensureSecret()`, which mints and writes `meta/catalog-secret` when it is absent (`secret.go`). `rocksdb.Config.ReadOnly` (`P/rocksdb/rocksdb.go`) opens with `grocksdb.OpenDbForReadOnlyColumnFamilies`, does not create the directory and does not flush on close.

Change:

1. Move the body of `Open` into `open(path, layout, txhashIndex, logger, readOnly bool)`. `Open` calls it with `false`.
2. Add:

   ```go
   // OpenReadOnly opens an existing catalog read-only. It fails when path holds no catalog and writes no file.
   func OpenReadOnly(path string, layout geometry.Layout, txhashIndex geometry.TxHashIndexLayout, logger *supportlog.Entry) (*Catalog, error)
   ```

3. `OpenReadOnly` first calls `os.Stat(path)`. On any error it returns `fmt.Errorf("catalog: open read-only %s: %w", path, err)`. This check runs before RocksDB, so a missing path never reaches the RocksDB env layer. Then it opens with `rocksdb.Config{Path: path, Logger: logger, ReadOnly: true}`.
4. The census runs as in `Open`. It is a scan and writes nothing.
5. Add `loadSecret() ([32]byte, bool, error)` in `secret.go`. `ensureSecret` calls it and mints only when it reports false. `OpenReadOnly` calls `loadSecret` and fails with `catalog: no cold-index secret at %s` when it is absent. Reason: every catalog that `catalog.Open` wrote has the secret, so an absent secret means the directory is not a finished catalog. Readers do not need the secret (facts F3), so the alternative "leave it zero" also works. Decide in PR if a reviewer prefers it; record the choice in the decision log.
6. Writes on a read-only catalog (`put`, `del`, `Batch`) return the RocksDB error. Add no extra guard. Document this on `OpenReadOnly`.

Open point to check first: `(*Catalog).NewSnapshot` calls `rocksdb.Store.NewSnapshot`. RocksDB supports snapshots on a read-only DB. The test in Section 6 proves it (`NewReadView` in PR 05 needs it).

### 4.3 Comments to rewrite

| File | Identifier | Today | After |
|---|---|---|---|
| `P/stores/hotchunk/hotchunk.go` | package doc | "A read-only open composes a ledgers-only view without the events facade (see OpenReadOnly)." | `OpenReadOnly` composes a ledgers-only view; `OpenReadOnlyWithEvents` composes all three facades. |
| same | `DB` doc | "a read-only open leaves events nil" | `OpenReadOnly` leaves events nil. |
| same | `OpenReadOnly` doc | keep the #834 reason; drop nothing else | add one line that points to `OpenReadOnlyWithEvents` for readers of events |
| same | `Events` doc | "(or #772 must add a warmed read-only variant)" | remove the #772 clause (#772 is a closed umbrella issue) |
| same | `open` inline comment | "A read-only open is a ledgers-only freeze/probe view" | describe `openMode.events` |
| `P/query/resolve.go` | `ReadView.Events` doc | "...because the registry holds read-write handles, whose events store is warmed (a read-only open would have none)." | a published handle must have events: a read-write open or `OpenReadOnlyWithEvents` |
| `P/query/registry.go` | `publishReadyHandles` doc | "(a read-only open is ledgers-only)" | remove the clause. The reason to open read-write is that the daemon's registry owns writer handles. |

### 4.4 File-set helper

New package `P/rpcv2test/fileset` (file `fileset.go`). It imports only the standard library and `testify/require`. It cannot live in `rpcv2test`: `rpcv2test` imports `catalog` and `hotchunk`, and the tests of both packages are internal (`package catalog`, `package hotchunk`), so that import is a cycle.

```go
type Entry struct { Size int64; ModTime time.Time; Mode fs.FileMode }
func Take(t testing.TB, root string) map[string]Entry        // walk with filepath.WalkDir; keys are paths relative to root; directories included
func RequireUnchanged(t testing.TB, root string, before map[string]Entry)
```

Directories are in the set, so a file that is created and then removed still changes the parent directory's mtime. PR 05 reuses the helper over the whole dataset root.

## 5. Files

| File | Change | What | Non-test lines |
|---|---|---|---|
| `P/stores/hotchunk/hotchunk.go` | modify | `openMode`, `OpenReadOnlyWithEvents`, `DB.readOnly`, `IngestLedger` guard, comments | 40 |
| `P/catalog/catalog.go` | modify | `open`, `OpenReadOnly`, doc | 30 |
| `P/catalog/secret.go` | modify | `loadSecret`; `ensureSecret` uses it | 10 |
| `P/query/resolve.go` | modify | `ReadView.Events` doc | 3 |
| `P/query/registry.go` | modify | `publishReadyHandles` doc | 3 |
| `P/rpcv2test/fileset/fileset.go` | new | `Entry`, `Take`, `RequireUnchanged` | 35 |
| `P/stores/hotchunk/hotchunk_test.go` | modify | new tests | test |
| `P/catalog/catalog_readonly_test.go` | new | new tests | test |
| `P/rpcv2test/fileset/fileset_test.go` | new | helper test | test |

## 6. Tests

| Test | Package/file | Proves | How |
|---|---|---|---|
| `TestOpenReadOnlyWithEvents_FileSetUnchanged` | `hotchunk`, `hotchunk_test.go` | Open, reads (events included) and close leave the directory's names, sizes and mtimes unchanged. | `Open(dir, chunk.ID(0), silentLogger())`; ingest 3 ledgers with `lcmWithEvent` through `ingestRaw`; `Close`. `before := fileset.Take(t, dir)`. Open with `OpenReadOnlyWithEvents`. Read: `MaxCommittedSeq`, `Ledgers().WithLedger`, `Ledgers().IterateLedgers`, `Txhash().Get(hash)`, `Events().EventCount`, `Events().Offsets`, `Events().LookupKeys(ctx, []event.TermKey{key})`, `Events().FetchEvents`, `Events().FetchRange`. `Close`. `fileset.RequireUnchanged(t, dir, before)`. |
| `TestOpenReadOnlyWithEvents_MatchesWriteOpen` | same | The read-only events facade returns the same data as a read-write open. | Same dataset. Compare `EventCount` (3), the `LookupKeys` bitmap and `FetchEvents` payloads with the values from `OpenExisting`. Close the read-write handle before the read-only open. |
| `TestOpenReadOnlyWithEvents_RejectsWrites` | same | `IngestLedger` fails before the batch; no file changes. | `ingestRaw(t, ro, first+3, zeroTxLCM(t, first+3))` returns the read-only error; `RequireUnchanged` after `Close`. |
| `TestOpenReadOnlyWithEvents_MissingDir` | same | A missing directory fails and is not created. | Path `filepath.Join(t.TempDir(), "absent")`; `require.Error`; `require.NoDirExists`. |
| `TestOpenReadOnly_SkipsEventsWarmup` (existing) | same | `OpenReadOnly` is still ledgers-only. | No change. |
| `TestOpenReadOnly_FileSetUnchanged` | `catalog`, `catalog_readonly_test.go` | Open, reads and close leave the catalog directory unchanged. | `path := filepath.Join(t.TempDir(), "rocksdb")`; `Open` with `geometry.NewLayout(t.TempDir())` and `geometry.NewTxHashIndexLayout(geometry.ChunksPerTxhashIndex)`; `PinEarliestLedger(chunk.ID(1).FirstLedger())`; `FlipHotReady(chunk.ID(2))`; `Close`. `Take`. `OpenReadOnly`. Read: `EarliestLedger`, `ReadyHotChunkKeys`, `HotState`, `NewSnapshot` then `LastCompleteChunk` then `Release`, `Secret` (equal to the first open). `Close`. `RequireUnchanged`. |
| `TestOpenReadOnly_MissingCatalog` | same | Spec: fails on a missing catalog and writes no file. | Parent `p := t.TempDir()`; `Take(t, p)`; `OpenReadOnly(filepath.Join(p, "catalog", "rocksdb"), ...)` returns an error that wraps `fs.ErrNotExist`; `RequireUnchanged(t, p, before)`. |
| `TestOpenReadOnly_RejectsWrites` | same | A write returns an error and changes no file. | `FlipHotReady` on the read-only catalog returns an error. |
| `TestOpenReadOnly_RefusesForeignKey` | same | The census still runs. | Open read-write, write a foreign key with `c.put`, close (as `reopenAfter` in `census_test.go` does); `OpenReadOnly` returns `errors.Is(err, ErrForeignCatalog)`. |
| `TestOpenReadOnly_NoSecret` | same | A catalog without the secret is refused (Section 4.2 item 5). | Open read-write, `c.del(catalogSecretStoreKey)`, close; `OpenReadOnly` fails with the no-secret error. |
| `TestTakeDetectsChanges` | `fileset`, `fileset_test.go` | The helper detects a new file, a removed file, a size change and an mtime change. | Plain files in `t.TempDir()`. Use a failing `testing.TB` stub to assert that `RequireUnchanged` fails. |

## 7. Done when

| Spec item | Check |
|---|---|
| The file-set test passes on a hot chunk directory. | `go test -race -run 'TestOpenReadOnlyWithEvents' ./cmd/stellar-rpc/internal/rpcv2/stores/hotchunk/` passes. |
| `catalog.OpenReadOnly` fails on a missing catalog and writes no file. | `go test -race -run 'TestOpenReadOnly_' ./cmd/stellar-rpc/internal/rpcv2/catalog/` passes, including `TestOpenReadOnly_MissingCatalog`. |
| Ledgers-only comments removed (D5). | `grep -rn 'ledgers-only' cmd/stellar-rpc/internal/rpcv2/query` returns nothing. In `hotchunk.go`, every match names `OpenReadOnly`. |
| Existing behaviour kept. | `go test -race ./cmd/stellar-rpc/internal/rpcv2/...` passes, including `TestOpenReadOnly_SkipsEventsWarmup` and the `backfill` hot-source tests. |

## 8. Verification before push

```
go build ./...
go vet ./...
go test -race ./cmd/stellar-rpc/internal/rpcv2/stores/hotchunk/... ./cmd/stellar-rpc/internal/rpcv2/catalog/... \
  ./cmd/stellar-rpc/internal/rpcv2/rpcv2test/... ./cmd/stellar-rpc/internal/rpcv2/query/... ./cmd/stellar-rpc/internal/rpcv2/backfill/...
go test -race ./cmd/stellar-rpc/internal/rpcv2/
make go-check-branch BASE=feature/full-history
git diff --stat feature/full-history -- . ':!*_test.go' ':!*.md'   # at or under 600 lines
```

## 9. Risks and open points

- Check first: RocksDB snapshots on a read-only DB. `TestOpenReadOnly_FileSetUnchanged` calls `NewSnapshot`. If it fails, stop and report; PR 05 needs it.
- Check first: a read-only RocksDB open of an existing directory writes no info LOG file. The facts probe (S1) says so for a hot chunk directory. The catalog test proves it for the catalog (default options, no per-CF tuning).
- The `os.Stat` pre-check in `OpenReadOnly` covers a missing path. An existing empty directory reaches RocksDB, which fails on the missing `CURRENT`. Add a test for the empty directory if the reviewer asks; check that it writes no file.
- Decision to record in the decision log in this PR: `OpenReadOnly` refuses a catalog without `meta/catalog-secret` (Section 4.2 item 5), or the alternative if the review changes it.
- The warmup cost moves to the open. `bench-serve` opens N ready chunks at start; each open scans two CFs. For a 10,000-ledger hot chunk this is not measured yet. PR 05 logs the open time.
- Nothing stops a caller from opening a chunk read-only while a writer holds it. Keep the doc warning on `OpenReadOnlyWithEvents` and on `OpenReadOnly`.
