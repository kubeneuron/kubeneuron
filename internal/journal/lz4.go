// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/pierrec/lz4/v4"
)

// Journal LZ4 payloads start with a little-endian uint64 size followed by
// a raw LZ4 block, rather than the LZ4 frame format.
func readLZ4(payload *io.SectionReader) ([]byte, error) {
	var header [8]byte
	if _, err := payload.ReadAt(header[:], 0); err != nil {
		return nil, err
	}
	size := binary.LittleEndian.Uint64(header[:])
	compressed := payload.Size() - 8
	if size == 0 || size > MaxFieldBytes || compressed <= 0 || compressed > int64(lz4.CompressBlockBound(int(size))) {
		return nil, fmt.Errorf("%w: invalid LZ4 field size", ErrInvalidJournal)
	}
	block := make([]byte, compressed)
	if _, err := payload.ReadAt(block, 8); err != nil {
		return nil, err
	}
	decoded := make([]byte, int(size))
	n, err := lz4.UncompressBlock(block, decoded)
	if err != nil || n != len(decoded) {
		return nil, fmt.Errorf("%w: invalid LZ4 block", ErrInvalidJournal)
	}
	return decoded, nil
}
