// SPDX-License-Identifier: Apache-2.0

// Package journal reads the systemd journal format described at
// https://systemd.io/JOURNAL_FILE_FORMAT/.
package journal

import (
	"encoding/binary"
	"errors"
	"io"
)

type fileHeader struct {
	start, end, tail uint64
	compact          bool
}

// readHeader checks the fixed header before offsets can be used to read objects.
func readHeader(r io.ReaderAt, fileSize int64) (fileHeader, error) {
	var header [208]byte
	if _, err := r.ReadAt(header[:], 0); err != nil {
		return fileHeader{}, err
	}
	if string(header[:8]) != "LPKSHHRH" || header[16] > 2 {
		return fileHeader{}, errors.New("invalid journal signature or state")
	}
	const knownIncompatibleFlags = 0x1f // XZ, LZ4, keyed hash, Zstandard, compact
	if binary.LittleEndian.Uint32(header[12:])&^knownIncompatibleFlags != 0 {
		return fileHeader{}, errors.New("unsupported journal features")
	}
	size := binary.LittleEndian.Uint64(header[88:])
	arena := binary.LittleEndian.Uint64(header[96:])
	tail := binary.LittleEndian.Uint64(header[136:])
	if fileSize < 0 || size < uint64(len(header)) || size%8 != 0 || size > uint64(fileSize) || arena > uint64(fileSize)-size {
		return fileHeader{}, errors.New("invalid journal arena")
	}
	if tail != 0 && (tail%8 != 0 || tail < size || tail > size+arena || size+arena-tail < 16) {
		return fileHeader{}, errors.New("invalid journal tail offset")
	}
	return fileHeader{start: size, end: size + arena, tail: tail, compact: header[12]&16 != 0}, nil
}
