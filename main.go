// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright 2026 John Wooten

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/stellar/go-stellar-sdk/support/compressxdr"
	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"
	"golang.org/x/sys/unix"
)

const configJSON = `{"networkPassphrase":"Public Global Stellar Network ; September 2015","version":"1.0","compression":"zstd","ledgersPerBatch":64,"batchesPerPartition":1000}`

type checkpoint struct {
	Version         int    `json:"version"`
	Input           string `json:"input"`
	Device          uint64 `json:"device"`
	Inode           uint64 `json:"inode"`
	CommittedOffset int64  `json:"committed_offset"`
	PunchedThrough  int64  `json:"punched_through"`
	LastExported    uint32 `json:"last_exported_ledger"`
	UpdatedAt       string `json:"updated_at"`
}

type options struct {
	input, output, state string
	follow, reclaim      bool
	poll                 time.Duration
}

func main() {
	var o options
	flag.StringVar(&o.input, "input", "", "Core METADATA_OUTPUT_STREAM file")
	flag.StringVar(&o.output, "output", "", "Galexie-compatible filesystem datastore")
	flag.StringVar(&o.state, "state", "", "checkpoint file (default: OUTPUT/.metadata-exporter-state.json)")
	flag.BoolVar(&o.follow, "follow", true, "wait for appended metadata")
	flag.BoolVar(&o.reclaim, "reclaim", true, "punch holes in durably consumed input extents")
	flag.DurationVar(&o.poll, "poll", time.Second, "follow polling interval")
	flag.Parse()
	if o.input == "" || o.output == "" {
		flag.Usage()
		os.Exit(2)
	}
	if o.state == "" {
		o.state = filepath.Join(o.output, ".metadata-exporter-state.json")
	}
	if err := run(o); err != nil {
		log.Fatal(err)
	}
}

func run(o options) error {
	if err := ensureOutput(o.output); err != nil {
		return err
	}
	f, err := waitOpen(o.input, o.follow, o.poll, o.reclaim)
	if err != nil {
		return err
	}
	defer f.Close()

	dev, ino, err := fileIdentity(f)
	if err != nil {
		return err
	}
	cp, err := loadCheckpoint(o.state)
	if err != nil {
		return err
	}
	if cp.Version == 0 {
		cp = checkpoint{Version: 1, Input: o.input, Device: dev, Inode: ino}
	} else if cp.Input != o.input || cp.Device != dev || cp.Inode != ino {
		return fmt.Errorf("input identity changed; refusing to reuse checkpoint (old dev/inode %d/%d, new %d/%d)", cp.Device, cp.Inode, dev, ino)
	}
	if _, err := f.Seek(cp.CommittedOffset, io.SeekStart); err != nil {
		return err
	}

	schema := datastore.DataStoreSchema{LedgersPerFile: 64, FilesPerPartition: 1000}
	var batch *xdr.LedgerCloseMetaBatch
	batchStartOffset := cp.CommittedOffset
	for {
		recordStart, _ := f.Seek(0, io.SeekCurrent)
		payload, recordBytes, err := readRecord(f)
		recordEnd := recordStart + recordBytes
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			if _, seekErr := f.Seek(recordStart, io.SeekStart); seekErr != nil {
				return seekErr
			}
			if !o.follow {
				if batch != nil {
					log.Printf("EOF with incomplete batch %d-%d (%d ledgers); checkpoint remains at byte %d", batch.StartSequence, batch.EndSequence, len(batch.LedgerCloseMetas), cp.CommittedOffset)
				}
				return nil
			}
			time.Sleep(o.poll)
			continue
		}
		if err != nil {
			return fmt.Errorf("read record at byte %d: %w", recordStart, err)
		}
		var meta xdr.LedgerCloseMeta
		if err := xdr.SafeUnmarshal(payload, &meta); err != nil {
			return fmt.Errorf("decode LedgerCloseMeta at byte %d: %w", recordStart, err)
		}
		seq := meta.LedgerSequence()

		if batch == nil {
			start := schema.GetSequenceNumberStartBoundary(seq)
			if seq != start {
				cp.CommittedOffset = recordEnd
				if err := commitAndReclaim(f, o, &cp); err != nil {
					return err
				}
				log.Printf("skipping ledger %d while waiting for 64-ledger boundary %d", seq, start+64)
				continue
			}
			batchStartOffset = recordStart
			batch = &xdr.LedgerCloseMetaBatch{StartSequence: xdr.Uint32(start), EndSequence: xdr.Uint32(schema.GetSequenceNumberEndBoundary(seq))}
		}

		expected := uint32(batch.StartSequence) + uint32(len(batch.LedgerCloseMetas))
		if seq != expected {
			log.Printf("gap: expected ledger %d, got %d; discarding incomplete batch and seeking next boundary", expected, seq)
			batch = nil
			if _, err := f.Seek(recordStart, io.SeekStart); err != nil {
				return err
			}
			cp.CommittedOffset = batchStartOffset
			continue
		}
		if err := batch.AddLedger(meta); err != nil {
			return fmt.Errorf("add ledger %d: %w", seq, err)
		}
		if seq != uint32(batch.EndSequence) {
			continue
		}
		key := schema.GetObjectKeyFromSequenceNumber(seq)
		if err := writeBatch(o.output, key, batch); err != nil {
			return err
		}
		cp.CommittedOffset = recordEnd
		cp.LastExported = seq
		if err := commitAndReclaim(f, o, &cp); err != nil {
			return err
		}
		log.Printf("exported %d-%d to %s (input byte %d)", batch.StartSequence, batch.EndSequence, key, recordEnd)
		batch = nil
		batchStartOffset = recordEnd
	}
}

func readRecord(r io.Reader) ([]byte, int64, error) {
	var payload bytes.Buffer
	var consumed int64
	for {
		var header [4]byte
		n, err := io.ReadFull(r, header[:])
		consumed += int64(n)
		if err != nil {
			return nil, consumed, err
		}
		mark := binary.BigEndian.Uint32(header[:])
		last := mark&0x80000000 != 0
		length := int64(mark & 0x7fffffff)
		if length > 1<<30 {
			return nil, consumed, fmt.Errorf("implausible fragment size %d", length)
		}
		n64, err := io.CopyN(&payload, r, length)
		consumed += n64
		if err != nil {
			return nil, consumed, err
		}
		if last {
			break
		}
	}
	return payload.Bytes(), consumed, nil
}

func writeBatch(root, key string, batch *xdr.LedgerCloseMetaBatch) error {
	path := filepath.Join(root, filepath.FromSlash(key))
	if existing, err := os.Open(path); err == nil {
		defer existing.Close()
		var found xdr.LedgerCloseMetaBatch
		if _, err := compressxdr.NewXDRDecoder(compressxdr.DefaultCompressor, &found).ReadFrom(existing); err != nil {
			return fmt.Errorf("existing batch %s is invalid: %w", path, err)
		}
		if found.StartSequence != batch.StartSequence || found.EndSequence != batch.EndSequence || len(found.LedgerCloseMetas) != len(batch.LedgerCloseMetas) {
			return fmt.Errorf("existing batch %s does not match %d-%d", path, batch.StartSequence, batch.EndSequence)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".metadata-batch-*.partial")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := compressxdr.NewXDREncoder(compressxdr.DefaultCompressor, batch).WriteTo(tmp); err != nil {
		return err
	}
	if err := tmp.Chmod(0644); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	ok = true
	return nil
}

func commitAndReclaim(input *os.File, o options, cp *checkpoint) error {
	cp.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveCheckpoint(o.state, cp); err != nil {
		return err
	}
	if !o.reclaim {
		return nil
	}
	const block = int64(4096)
	end := cp.CommittedOffset / block * block
	if end <= cp.PunchedThrough {
		return nil
	}
	if err := unix.Fallocate(int(input.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, cp.PunchedThrough, end-cp.PunchedThrough); err != nil {
		return fmt.Errorf("reclaim input bytes %d-%d (rerun with --reclaim=false if filesystem lacks hole punching): %w", cp.PunchedThrough, end, err)
	}
	cp.PunchedThrough = end
	return saveCheckpoint(o.state, cp)
}

func ensureOutput(root string) error {
	if err := os.MkdirAll(root, 0755); err != nil {
		return err
	}
	path := filepath.Join(root, ".config.json")
	b, err := os.ReadFile(path)
	if err == nil {
		var got, want any
		if json.Unmarshal(b, &got) != nil || json.Unmarshal([]byte(configJSON), &want) != nil || fmt.Sprint(got) != fmt.Sprint(want) {
			return fmt.Errorf("existing %s is not the required Galexie schema", path)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := atomicWrite(path, append([]byte(configJSON), '\n')); err != nil {
		return err
	}
	return os.Chmod(path, 0644)
}

func loadCheckpoint(path string) (checkpoint, error) {
	var cp checkpoint
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cp, nil
	}
	if err != nil {
		return cp, err
	}
	if err := json.Unmarshal(b, &cp); err != nil {
		return cp, fmt.Errorf("decode checkpoint: %w", err)
	}
	return cp, nil
}

func saveCheckpoint(path string, cp *checkpoint) error {
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'))
}

func atomicWrite(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.partial")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func waitOpen(path string, follow bool, poll time.Duration, writable bool) (*os.File, error) {
	flags := os.O_RDONLY
	if writable {
		flags = os.O_RDWR
	}
	for {
		f, err := os.OpenFile(path, flags, 0)
		if err == nil {
			return f, nil
		}
		if !follow || !os.IsNotExist(err) {
			return nil, err
		}
		log.Printf("waiting for metadata stream %s", path)
		time.Sleep(poll)
	}
}

func fileIdentity(f *os.File) (uint64, uint64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("cannot read input device/inode")
	}
	return uint64(st.Dev), st.Ino, nil
}
