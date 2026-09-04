// Package bench benchmarks full-history storage from both sides: writes
// (bench-ingest) and reads (bench-query). Each side has a cold and a hot
// subcommand, since the two tiers are different code paths.
//
// An ingest run drives the daemon's production ingestion code over a
// benchmark-controlled ledger source: cold calls backfill.RunBackfill, hot the
// production ingestion loop. A query run reads a dataset an ingest run left on
// disk, rebuilds the catalog state those artifacts imply, and issues each query
// type at each arrival rate through query.ReadView, so routing resolves a
// chunk's tier as a served request does.
//
// Either way a csvSink collects the signals and aggregates the run into
// percentile CSV reports, laid out by the schema that run declared.
package bench
