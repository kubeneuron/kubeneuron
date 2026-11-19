// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"encoding/binary"
	"errors"
	"io"
)

type dataObject struct {
	firstEntry, entryArray, entries uint64
	flags                           byte
	payload                         *io.SectionReader
}

func (h fileHeader) dataAt(r io.ReaderAt, offset uint64) (dataObject, error) {
	kind, flags, body, err := h.objectAt(r, offset)
	if err != nil {
		return dataObject{}, err
	}
	if kind != 1 {
		return dataObject{}, errors.New("expected journal DATA object")
	}
	var fixed [48]byte
	payloadStart := int64(len(fixed))
	if h.compact {
		payloadStart += 8
	}
	if body.Size() <= payloadStart {
		return dataObject{}, errors.New("missing journal DATA payload")
	}
	if _, err := body.ReadAt(fixed[:], 0); err != nil {
		return dataObject{}, err
	}
	return dataObject{
		firstEntry: binary.LittleEndian.Uint64(fixed[24:]),
		entryArray: binary.LittleEndian.Uint64(fixed[32:]),
		entries:    binary.LittleEndian.Uint64(fixed[40:]),
		flags:      flags,
		payload:    io.NewSectionReader(body, payloadStart, body.Size()-payloadStart),
	}, nil
}
