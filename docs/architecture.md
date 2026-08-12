# Architecture and data format

## Purpose

Stellar Galexie normally starts Captive Core to obtain `LedgerCloseMeta` (LCM)
records. This exporter is for operators who already run a passive, synced
Stellar Core observer and want to reuse that Core's metadata stream instead of
running another Core process.

```text
Stellar network
      │
      ▼
observer stellar-core
      │  RFC 5531-framed LedgerCloseMeta XDR
      ▼
append-only metadata stream file
      │
      ▼
stellar-core-metadata-exporter
      │  64-ledger LedgerCloseMetaBatch + zstd
      ▼
Galexie-compatible filesystem datastore
      │
      └──► CDP/Hubble-compatible consumer
```

The exporter replaces only Galexie's Captive Core and batching stage. It does
not replace a downstream processor, database loader, or Hubble deployment.

## Input format

Set `METADATA_OUTPUT_STREAM` on Stellar Core to a regular file. Core appends one
`LedgerCloseMeta` XDR object for each ledger it applies, during both catch-up and
live operation. Each object uses the record-marking format from RFC 5531:

1. a four-byte big-endian fragment header;
2. bit 31 set on the final fragment;
3. bits 0–30 containing the fragment length; and
4. one or more fragments containing the XDR payload.

The exporter accepts multi-fragment records and rejects fragments larger than
1 GiB. An incomplete final record is treated as a writer-in-progress condition
in follow mode and is retried from its starting offset.

## Output format

Output uses the same filesystem key scheme and XDR envelope as Galexie:

- `LedgerCloseMetaBatch` XDR;
- zstd compression;
- 64 consecutive ledgers per batch;
- 1,000 batches per partition; and
- a `.config.json` schema marker in the datastore root.

An object path resembles:

```text
FC3163FF--63872000-63935999/FC30ABBF--63919168-63919231.xdr.zst
```

The hexadecimal prefix sorts newer ledger ranges before older ranges, matching
the Stellar SDK datastore schema. Consumers should use the schema rather than
constructing paths themselves.

The current schema is fixed to Stellar Pubnet:

```json
{
  "networkPassphrase": "Public Global Stellar Network ; September 2015",
  "version": "1.0",
  "compression": "zstd",
  "ledgersPerBatch": 64,
  "batchesPerPartition": 1000
}
```

## Alignment and gaps

A Galexie batch begins at a sequence divisible by 64. When the first stream
record is inside an already-started range, the exporter skips records until the
next boundary. For example, a stream beginning at ledger 101 starts publishing
at ledger 128.

Within a batch, ledger sequences must be strictly consecutive. If the exporter
expects ledger 150 and receives ledger 151, it discards the incomplete batch and
waits for the next 64-ledger boundary. It never publishes a knowingly partial
batch.

This behavior preserves datastore validity but does not backfill missing
history. Use Galexie/Captive Core or another historical source to fill ranges
that were not emitted by the observer.

## Durability and replay

For each completed batch, the exporter:

1. encodes into a temporary file in the destination directory;
2. sets mode `0644`;
3. flushes the file;
4. atomically renames it to its final object key;
5. flushes the containing directory;
6. atomically saves the committed input offset; and
7. optionally reclaims committed input extents.

If the process stops after the output rename but before checkpointing, it
replays that input after restart. An existing output batch is decoded and its
range and ledger count are checked before the checkpoint advances. This makes
normal crash replay idempotent.

## Sparse input reclamation

Core keeps its metadata stream open and appends across ledger closes. Truncating
that open file would leave Core's file offset beyond the new end and could make
a large sparse gap. Instead, the exporter uses Linux
`FALLOC_FL_PUNCH_HOLE | FALLOC_FL_KEEP_SIZE` on fully committed 4 KiB extents.

Consequences:

- the inode and append offset remain stable;
- `ls -lh` reports a continually growing logical size;
- `du -h` reports the much smaller allocated size; and
- the checkpoint is required because consumed bytes read back as zeroes.

Reclamation requires a filesystem that supports hole punching and write access
to the input file. Use `--reclaim=false` for read-only inputs, portability, or
initial validation.

## Resource model

The exporter performs one XDR decode per ledger and one zstd encode per 64
ledgers. Its pending batch is held in memory, so memory usage depends on current
LCM sizes. It does not start Stellar Core, maintain ledger state, or perform
historical catch-up independently.

The Core write remains synchronous. A regular file decouples Core from exporter
availability better than a FIFO: Core can keep appending while the exporter is
stopped, subject to available disk space.
