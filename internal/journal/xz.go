// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"fmt"
	"io"

	"github.com/xi2/xz"
)

func readXZ(payload *io.SectionReader) ([]byte, error) {
	if payload.Size() == 0 || payload.Size() > MaxFieldBytes {
		return nil, fmt.Errorf("%w: invalid XZ field size", ErrInvalidJournal)
	}
	block := make([]byte, payload.Size())
	if _, err := payload.ReadAt(block, 0); err != nil {
		return nil, err
	}
	const maxDictionary = 8 << 20
	reader, err := xz.NewReader(bytes.NewReader(block), maxDictionary)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid XZ header: %v", ErrInvalidJournal, err)
	}
	field, err := io.ReadAll(io.LimitReader(reader, MaxFieldBytes+1))
	if err != nil || len(field) > MaxFieldBytes {
		return nil, fmt.Errorf("%w: invalid or oversized XZ field", ErrInvalidJournal)
	}
	return field, nil
}
