# Stellar Core Galexie Exporter

[![Tests](https://github.com/JFWooten4/stellar-core-galexie-exporter/actions/workflows/test.yml/badge.svg)](https://github.com/JFWooten4/stellar-core-galexie-exporter/actions/workflows/test.yml)

Consumes Stellar Core's RFC 5531-framed `METADATA_OUTPUT_STREAM` and writes the
same 64-ledger, zstd-compressed `LedgerCloseMetaBatch` filesystem layout used by
Galexie. The output is suitable for Galexie/Hubble-compatible readers without
running a second Captive Core instance.

The exporter commits output by atomic rename, then atomically checkpoints the
input byte offset. By default it hole-punches only fully consumed 4 KiB extents,
so the append-only Core stream keeps a stable inode and logical offset without
continually consuming SSD blocks.

## Documentation

- [Architecture and data format](docs/architecture.md)
- [Installation and service setup](docs/installation.md)
- [Operations, recovery, and troubleshooting](docs/operations.md)

## Usage

Configure an observer Stellar Core node:

```ini
NODE_IS_VALIDATOR=false
METADATA_OUTPUT_STREAM="/var/lib/stellar/metadata-output/ledger-close-meta.xdr"
```

Then run the exporter:

```bash
stellar-core-metadata-exporter \
  --input /mnt/core/stellar/core-data/metadata-output/ledger-close-meta.xdr \
  --output /mnt/rpc/stellar-processing/galexie-main-core
```

Run `stellar-core-metadata-exporter -h` for all options. The current release is
Linux-only when reclaim is enabled and writes the fixed Pubnet Galexie schema of
64 ledgers per batch and 1,000 batches per partition.

The first partial 64-ledger range is intentionally skipped. Output starts at the
next sequence divisible by 64. A sequence gap discards the incomplete batch and
waits for the next boundary rather than publishing corrupt history.

Do not delete `.metadata-exporter-state.json`; it is required to traverse the
sparse input stream after consumed extents have been reclaimed. Disable reclaim
with `--reclaim=false` when testing against an irreplaceable input file.

## Validator safety

Stellar Core rejects `METADATA_OUTPUT_STREAM` when `NODE_IS_VALIDATOR=true`
because metadata is written synchronously during ledger close. Run this tool
only against a passive observer node.

## Development

Run the test and static-analysis suite before submitting a change:

```bash
gofmt -w main.go main_test.go
go test -race -cover ./...
go vet ./...
go build -trimpath ./...
```

GitHub Actions runs the same checks for pull requests and pushes to `main`.
