# Operations, recovery, and troubleshooting

## Healthy operation

The exporter writes progress to standard error. Under systemd:

```bash
journalctl -u stellar-core-galexie-exporter.service -f
```

Expected messages include:

```text
skipping ledger 63919167 while waiting for 64-ledger boundary 63919168
exported 63919168-63919231 to .../FC30ABBF--63919168-63919231.xdr.zst
```

The first message is normal only during initial alignment or after a detected
gap. Once aligned, one export message should appear every 64 applied ledgers.

## Check lag and storage

Read the durable progress marker:

```bash
jq . /var/lib/galexie/main-core/.metadata-exporter-state.json
```

Important fields:

| Field | Meaning |
|---|---|
| `committed_offset` | Input byte offset safe to resume from |
| `punched_through` | Input prefix already hole-punched |
| `last_exported_ledger` | End ledger of the latest complete batch |
| `device` / `inode` | Identity of the input stream file |
| `updated_at` | Last durable checkpoint time in UTC |

Compare logical and allocated input sizes:

```bash
ls -lh /var/lib/stellar/metadata-output/ledger-close-meta.xdr
du -h /var/lib/stellar/metadata-output/ledger-close-meta.xdr
```

A large `ls` size with a small `du` size is expected after reclamation. Alert on
allocated size growth, stale `updated_at`, repeated gap messages, service
restarts, or low free space.

## Restart behavior

The exporter can be restarted normally:

```bash
sudo systemctl restart stellar-core-galexie-exporter.service
```

It seeks to `committed_offset` and rebuilds any incomplete in-memory batch from
the unreclaimed input suffix. Do not edit the checkpoint while the service is
running.

Stellar Core opens a regular metadata file in append mode. A normal Core restart
therefore preserves the stream inode and prior records.

## Important file rules

Do not:

- delete or replace the input stream while its checkpoint exists;
- copy-truncate the input stream;
- delete `.metadata-exporter-state.json` after reclaiming input extents;
- point two exporters at the same output datastore; or
- let another writer create the same batch keys concurrently.

The exporter refuses to reuse a checkpoint when the input path, device, or
inode changes. This prevents it from silently applying an old byte offset to a
different stream.

## Input identity changed

Example error:

```text
input identity changed; refusing to reuse checkpoint
```

This means the stream file was replaced, moved across filesystems, or recreated.
Do not simply edit the saved inode. Choose one of these recovery paths:

1. Restore the original stream file/inode and restart the exporter.
2. Preserve the existing datastore, choose a new output datastore and state
   file, and begin at the next 64-ledger boundary.
3. Reconstruct the missing range with Galexie/Captive Core, then begin a new
   stream at an aligned boundary.

Because reclaimed records contain holes, deleting the checkpoint usually makes
the old stream unreadable from byte zero.

## Hole punching failed

Example error:

```text
reclaim input bytes ...: operation not supported
```

The filesystem may not support hole punching, the input may be read-only, or
permissions may prevent modification. Restart with:

```bash
--reclaim=false
```

This preserves export functionality but the raw stream's allocated disk usage
will grow until an operator performs a coordinated rotation while Core is
stopped.

## Output schema mismatch

The exporter validates an existing `.config.json` before reading input. It
stops if that file differs from its fixed Pubnet/64/1000 schema. Use a separate
empty output directory rather than mixing schemas.

## Existing batch is invalid

On replay, an existing destination object is decoded. The exporter stops if it
cannot decode it or if its declared range/count differs from the pending batch.
Investigate the object before moving it aside or rebuilding it. Never overwrite
an unexplained mismatch automatically.

## Sequence gap

Example message:

```text
gap: expected ledger 63919200, got 63919201
```

No partial object is published. The exporter abandons that batch and scans for
the next boundary. Record the missing range and backfill it separately if the
datastore must be continuous.

## End-of-file behavior

With `--follow=true`, EOF and an incomplete final record are retried after the
poll interval. This is normal while Core is between ledger closes.

With `--follow=false`, the exporter exits cleanly at EOF and logs any incomplete
batch. This mode is useful for offline validation and tests.

## Backups

Back up these items together:

- the Galexie datastore;
- `.metadata-exporter-state.json`; and
- the uncommitted suffix of the metadata stream.

The datastore alone preserves complete published batches. The state and raw
suffix are required to resume the next incomplete batch without waiting for a
new boundary.

## Hubble/CDP integration

The output objects are Galexie-compatible `LedgerCloseMetaBatch` files. A
downstream consumer must be configured to read the filesystem datastore or the
objects must be synchronized to a datastore supported by that consumer.

This exporter does not launch Hubble, transform metadata into relational rows,
or manage downstream consumer checkpoints. Validate datastore continuity with
Galexie's `detect-gaps` before advancing a production consumer.

## Current limitations

- Pubnet network passphrase is fixed.
- Batch and partition sizes are fixed at 64 and 1,000.
- Reclamation is Linux-specific.
- There is no historical backfill engine.
- There is no Prometheus or HTTP health endpoint.
- Input replacement/rotation is intentionally manual.
- Existing batch replay checks range and count, not a content hash comparison.
