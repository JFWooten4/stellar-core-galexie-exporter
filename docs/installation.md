# Installation and service setup

## Requirements

- Linux on `amd64` or `arm64`;
- Go 1.25 or newer to build;
- a Stellar Core observer with a writable regular-file
  `METADATA_OUTPUT_STREAM`;
- enough temporary disk space for unconsumed raw metadata and one compressed
  output batch; and
- a filesystem supporting hole punching when reclaim is enabled.

This release is fixed to Pubnet, 64 ledgers per batch, and 1,000 batches per
partition.

## Build

```bash
git clone https://github.com/JFWooten4/stellar-core-galexie-exporter.git
cd stellar-core-galexie-exporter
go build -trimpath -o stellar-core-metadata-exporter .
sudo install -m 0755 stellar-core-metadata-exporter \
  /usr/local/bin/stellar-core-metadata-exporter
```

Run the checks before deploying a modified build:

```bash
gofmt -w main.go
go test ./...
go vet ./...
```

## Configure Stellar Core

Create the stream file on a volume with enough space for the data accumulated
while the exporter is unavailable:

```bash
sudo install -d -m 0755 -o stellar -g stellar \
  /var/lib/stellar/metadata-output
sudo install -m 0660 -o stellar -g stellar /dev/null \
  /var/lib/stellar/metadata-output/ledger-close-meta.xdr
```

Add these settings to `stellar-core.cfg`:

```ini
NODE_IS_VALIDATOR=false
METADATA_OUTPUT_STREAM="/var/lib/stellar/metadata-output/ledger-close-meta.xdr"
```

Restart Core once to open the configured output. Confirm that the file grows as
Core applies ledgers.

`METADATA_OUTPUT_STREAM` is mutually exclusive with
`NODE_IS_VALIDATOR=true`. Stellar Core enforces this because stream writes occur
synchronously during ledger close. Do not enable this setting on a voting
validator.

### Docker path mapping

When Core runs in Docker, the path in `stellar-core.cfg` is inside the
container. Bind the containing directory to the host and give the exporter the
host path. For example:

```yaml
services:
  core:
    volumes:
      - /mnt/core/stellar/core-data:/var/lib/stellar
```

The corresponding host input is:

```text
/mnt/core/stellar/core-data/metadata-output/ledger-close-meta.xdr
```

## Run manually

Start with reclaim disabled while validating paths and permissions:

```bash
stellar-core-metadata-exporter \
  --input /var/lib/stellar/metadata-output/ledger-close-meta.xdr \
  --output /var/lib/galexie/main-core \
  --reclaim=false
```

After validating at least one batch with Galexie, stop the process and restart
with `--reclaim=true`. The input must then be writable by the exporter.

Available options:

| Option | Default | Meaning |
|---|---:|---|
| `--input` | required | Core metadata stream file |
| `--output` | required | Galexie-compatible datastore root |
| `--state` | `OUTPUT/.metadata-exporter-state.json` | Durable checkpoint path |
| `--follow` | `true` | Wait for the writer at end-of-file |
| `--poll` | `1s` | End-of-file polling interval |
| `--reclaim` | `true` | Hole-punch durably consumed input extents |

## systemd service

Create `/etc/systemd/system/stellar-core-galexie-exporter.service`:

```ini
[Unit]
Description=Export Stellar Core metadata to Galexie batches
After=local-fs.target

[Service]
Type=simple
User=stellar-exporter
Group=stellar-exporter
ExecStart=/usr/local/bin/stellar-core-metadata-exporter \
  --input /var/lib/stellar/metadata-output/ledger-close-meta.xdr \
  --output /var/lib/galexie/main-core \
  --follow=true \
  --reclaim=true
Restart=always
RestartSec=3
Nice=10
CPUQuota=50%%
MemoryHigh=1500M
MemoryMax=2500M
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

Create the service account and grant it access to both paths according to your
distribution's account-management and ownership policy. Then enable the unit:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now stellar-core-galexie-exporter.service
sudo systemctl status stellar-core-galexie-exporter.service
```

The memory limits above are examples, not universal recommendations. Observe
LCM sizes on your workload before choosing a hard limit.

## Validate with Galexie

Point a Galexie filesystem configuration at the output datastore. The included
[`validation-galexie.toml`](../validation-galexie.toml) is a container-oriented
example where the datastore is mounted at `/data`.

```bash
stellar-galexie detect-gaps \
  --config-file validation-galexie.toml \
  --start 63919168 \
  --end 63919231
```

A completed batch should report 64 ledgers found and zero missing. Use actual
batch boundaries from your output directory.
