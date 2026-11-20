// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

func readZstd(payload *io.SectionReader) ([]byte, error) {
	// Journal writers use compression only when the encoded field is smaller.
	if payload.Size() > MaxFieldBytes || payload.Size() == 0 {
		return nil, fmt.Errorf("%w: invalid Zstandard field size", ErrInvalidJournal)
	}
	block := make([]byte, payload.Size())
	if _, err := payload.ReadAt(block, 0); err != nil {
		return nil, err
	}
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(MaxFieldBytes))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	field, err := decoder.DecodeAll(block, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid Zstandard field: %v", ErrInvalidJournal, err)
	}
	return field, nil
}
