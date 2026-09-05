// Package bench benchmarks full-history storage: writes (bench-ingest) and
// reads (bench-query). Each has a cold and a hot subcommand, one per tier.
//
// An ingest run drives the daemon's ingestion code over a benchmark-controlled
// ledger source: cold calls backfill.RunBackfill, hot the production ingestion
// loop. A query run reads a dataset an ingest run left on disk, rebuilds the
// catalog state the artifacts imply, and issues each query type at each
// arrival rate through query.ReadView.
//
// A csvSink collects the signals and aggregates the run into percentile CSV
// reports.
package bench
