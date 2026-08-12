// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright 2026 John Wooten

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/support/compressxdr"
	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func TestReadRecordSingleAndMultipleFragments(t *testing.T) {
	tests := []struct {
		name      string
		fragments [][]byte
	}{
		{name: "single", fragments: [][]byte{[]byte("ledger-meta")}},
		{name: "multiple", fragments: [][]byte{[]byte("ledger-"), []byte("close-"), []byte("meta")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			framed := frameFragments(t, tt.fragments...)
			got, consumed, err := readRecord(bytes.NewReader(framed))
			if err != nil {
				t.Fatalf("readRecord() error = %v", err)
			}
			want := bytes.Join(tt.fragments, nil)
			if !bytes.Equal(got, want) {
				t.Fatalf("readRecord() payload = %q, want %q", got, want)
			}
			if consumed != int64(len(framed)) {
				t.Fatalf("readRecord() consumed = %d, want %d", consumed, len(framed))
			}
		})
	}
}

func TestReadRecordIncompleteAndImplausible(t *testing.T) {
	var incomplete bytes.Buffer
	writeMark(t, &incomplete, true, 5)
	incomplete.WriteString("ab")
	_, consumed, err := readRecord(bytes.NewReader(incomplete.Bytes()))
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("readRecord() error = %v, want EOF or unexpected EOF", err)
	}
	if consumed != 6 {
		t.Fatalf("readRecord() consumed = %d, want 6", consumed)
	}

	var oversized bytes.Buffer
	writeMark(t, &oversized, true, (1<<30)+1)
	_, _, err = readRecord(bytes.NewReader(oversized.Bytes()))
	if err == nil || !strings.Contains(err.Error(), "implausible fragment size") {
		t.Fatalf("readRecord() error = %v, want implausible fragment size", err)
	}
}

func TestEnsureOutputCreatesAndProtectsSchema(t *testing.T) {
	root := filepath.Join(t.TempDir(), "datastore")
	if err := ensureOutput(root); err != nil {
		t.Fatalf("ensureOutput() error = %v", err)
	}

	configPath := filepath.Join(root, ".config.json")
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != configJSON {
		t.Fatalf("config = %s, want %s", got, configJSON)
	}
	if info, err := os.Stat(configPath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0644 {
		t.Fatalf("config mode = %o, want 644", info.Mode().Perm())
	}
	if err := ensureOutput(root); err != nil {
		t.Fatalf("ensureOutput() on matching schema error = %v", err)
	}

	if err := os.WriteFile(configPath, []byte(`{"networkPassphrase":"wrong"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ensureOutput(root); err == nil || !strings.Contains(err.Error(), "not the required Galexie schema") {
		t.Fatalf("ensureOutput() error = %v, want schema mismatch", err)
	}
}

func TestRunAlignsBatchesResumesAndWritesGalexieObjects(t *testing.T) {
	root := t.TempDir()
	inputPath := filepath.Join(root, "ledger-close-meta.xdr")
	outputPath := filepath.Join(root, "galexie")
	statePath := filepath.Join(outputPath, "checkpoint.json")

	// The exporter skips 127, then retains the incomplete 128-150 batch.
	writeMetaRange(t, inputPath, 127, 150, false)
	o := options{input: inputPath, output: outputPath, state: statePath, follow: false, reclaim: false}
	if err := run(o); err != nil {
		t.Fatalf("first run() error = %v", err)
	}
	cp, err := loadCheckpoint(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if cp.LastExported != 0 {
		t.Fatalf("last exported ledger = %d before complete batch, want 0", cp.LastExported)
	}

	writeMetaRange(t, inputPath, 151, 191, true)
	if err := run(o); err != nil {
		t.Fatalf("resume run() error = %v", err)
	}
	firstPath := batchPath(outputPath, 128)
	first := decodeBatch(t, firstPath)
	assertBatchRange(t, first, 128, 191)
	firstBytes, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}

	// Re-running at EOF must leave the already-published object unchanged.
	if err := run(o); err != nil {
		t.Fatalf("idempotent run() error = %v", err)
	}
	gotFirstBytes, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotFirstBytes, firstBytes) {
		t.Fatal("completed batch changed during EOF replay")
	}

	writeMetaRange(t, inputPath, 192, 255, true)
	if err := run(o); err != nil {
		t.Fatalf("second batch run() error = %v", err)
	}
	second := decodeBatch(t, batchPath(outputPath, 192))
	assertBatchRange(t, second, 192, 255)

	cp, err = loadCheckpoint(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if cp.LastExported != 255 {
		t.Fatalf("last exported ledger = %d, want 255", cp.LastExported)
	}
	if cp.CommittedOffset <= 0 || cp.UpdatedAt == "" {
		t.Fatalf("incomplete checkpoint: %+v", cp)
	}
}

func TestRunRejectsReplacedInput(t *testing.T) {
	root := t.TempDir()
	inputPath := filepath.Join(root, "ledger-close-meta.xdr")
	outputPath := filepath.Join(root, "galexie")
	statePath := filepath.Join(outputPath, "checkpoint.json")
	o := options{input: inputPath, output: outputPath, state: statePath, follow: false, reclaim: false}

	writeMetaRange(t, inputPath, 1, 1, false)
	if err := run(o); err != nil {
		t.Fatalf("initial run() error = %v", err)
	}
	if err := os.Rename(inputPath, inputPath+".old"); err != nil {
		t.Fatal(err)
	}
	writeMetaRange(t, inputPath, 2, 2, false)

	err := run(o)
	if err == nil || !strings.Contains(err.Error(), "input identity changed") {
		t.Fatalf("run() error = %v, want input identity changed", err)
	}
}

func TestWriteBatchValidatesExistingObject(t *testing.T) {
	root := t.TempDir()
	batch := makeBatch(t, 128, 191)
	key := datastore.DataStoreSchema{LedgersPerFile: 64, FilesPerPartition: 1000}.GetObjectKeyFromSequenceNumber(128)

	if err := writeBatch(root, key, batch); err != nil {
		t.Fatalf("writeBatch() error = %v", err)
	}
	if err := writeBatch(root, key, batch); err != nil {
		t.Fatalf("writeBatch() replay error = %v", err)
	}
	path := filepath.Join(root, filepath.FromSlash(key))
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0644 {
		t.Fatalf("batch mode = %o, want 644", info.Mode().Perm())
	}

	if err := os.WriteFile(path, []byte("corrupt"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeBatch(root, key, batch); err == nil || !strings.Contains(err.Error(), "is invalid") {
		t.Fatalf("writeBatch() error = %v, want invalid existing batch", err)
	}
}

func makeMeta(seq uint32) xdr.LedgerCloseMeta {
	return xdr.LedgerCloseMeta{
		V: 0,
		V0: &xdr.LedgerCloseMetaV0{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{
				Header: xdr.LedgerHeader{
					LedgerVersion: 27,
					LedgerSeq:     xdr.Uint32(seq),
					ScpValue:      xdr.StellarValue{CloseTime: xdr.TimePoint(seq)},
				},
			},
		},
	}
}

func makeBatch(t *testing.T, start, end uint32) *xdr.LedgerCloseMetaBatch {
	t.Helper()
	batch := &xdr.LedgerCloseMetaBatch{StartSequence: xdr.Uint32(start), EndSequence: xdr.Uint32(end)}
	for seq := start; seq <= end; seq++ {
		if err := batch.AddLedger(makeMeta(seq)); err != nil {
			t.Fatal(err)
		}
	}
	return batch
}

func writeMetaRange(t *testing.T, path string, start, end uint32, appendFile bool) {
	t.Helper()
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendFile {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	for seq := start; seq <= end; seq++ {
		var payload bytes.Buffer
		if _, err := xdr.Marshal(&payload, makeMeta(seq)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(frameFragments(t, payload.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
}

func frameFragments(t *testing.T, fragments ...[]byte) []byte {
	t.Helper()
	var framed bytes.Buffer
	for i, fragment := range fragments {
		writeMark(t, &framed, i == len(fragments)-1, uint32(len(fragment)))
		framed.Write(fragment)
	}
	return framed.Bytes()
}

func writeMark(t *testing.T, w io.Writer, last bool, length uint32) {
	t.Helper()
	mark := length
	if last {
		mark |= 0x80000000
	}
	if err := binary.Write(w, binary.BigEndian, mark); err != nil {
		t.Fatal(err)
	}
}

func batchPath(root string, seq uint32) string {
	schema := datastore.DataStoreSchema{LedgersPerFile: 64, FilesPerPartition: 1000}
	return filepath.Join(root, filepath.FromSlash(schema.GetObjectKeyFromSequenceNumber(seq)))
}

func decodeBatch(t *testing.T, path string) xdr.LedgerCloseMetaBatch {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var batch xdr.LedgerCloseMetaBatch
	if _, err := compressxdr.NewXDRDecoder(compressxdr.DefaultCompressor, &batch).ReadFrom(f); err != nil {
		t.Fatal(err)
	}
	return batch
}

func assertBatchRange(t *testing.T, batch xdr.LedgerCloseMetaBatch, start, end uint32) {
	t.Helper()
	if uint32(batch.StartSequence) != start || uint32(batch.EndSequence) != end {
		t.Fatalf("batch range = %d-%d, want %d-%d", batch.StartSequence, batch.EndSequence, start, end)
	}
	wantCount := int(end-start) + 1
	if len(batch.LedgerCloseMetas) != wantCount {
		t.Fatalf("batch count = %d, want %d", len(batch.LedgerCloseMetas), wantCount)
	}
	for i, meta := range batch.LedgerCloseMetas {
		want := start + uint32(i)
		if got := meta.LedgerSequence(); got != want {
			t.Fatalf("ledger[%d] = %d, want %d", i, got, want)
		}
	}
}
