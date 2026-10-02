// Package bench benchmarks full-history storage: writes (bench-ingest) and
// reads (bench-query). Each has a cold and a hot subcommand, one per tier.
//
// An ingest run drives the daemon's ingestion code over a benchmark-controlled
// ledger source: cold calls backfill.RunBackfill, hot the production ingestion
// loop. A query run reads a dataset an ingest run left on disk, rebuilds the
// catalog state the artifacts imply, and issues each query type at each
// arrival rate through query.ReadView.
// Query results measure storage read paths, not RPC endpoint SLAs: handler,
// response serialization and network work are excluded. Cache scenarios record
// requested controls, not a verified cache state. See README.md for semantics.
//
// A csvSink collects the signals and aggregates the run into percentile CSV
// reports.
package bench
