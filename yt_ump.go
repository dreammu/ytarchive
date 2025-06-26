package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

/*
	Info and code references taken from the following
	https://github.com/gsuberland/UMP_Format/blob/main/UMP_Format.md
	https://github.com/davidzeng0/innertube/blob/main/googlevideo/ump.md

	This is unused for the time being.
	Requires also figuring out downloading via adaptive formats,
	which requires nsig, which is not feasible in Go.
*/

type VarInt uint32

type UmpPart struct {
	Type VarInt
	Size VarInt
	Data []byte
}

const UMP_MEDIA_TYPE = 21

func VarIntSize(b byte) (uint32, error) {
	size := uint32(0)
	for shift := uint32(0); shift < 5; shift++ {
		if (b & (128 >> shift)) == 0 {
			size = shift + 1
			break
		}
	}

	if size < 1 || size > 5 {
		return size, fmt.Errorf("expected variable int size to be between 1 and 5, got %d", size)
	}

	return size, nil
}

func ReadVarInt(buf *bytes.Buffer) (VarInt, error) {
	prefix, err := buf.ReadByte()
	if err != nil {
		return 0, err
	}

	size, err := VarIntSize(prefix)
	if err != nil {
		return 0, err
	}

	shift := uint32(0)
	result := uint32(0)
	if size != 5 {
		shift = 8 - size
		mask := uint32((1 << shift) - 1)
		result = uint32(prefix) & mask
	}

	for i := uint32(1); i < size; i++ {
		b, err := buf.ReadByte()
		if err != nil {
			return 0, err
		}

		result |= uint32(b) << shift
		shift += 8
	}

	return VarInt(result), nil
}

func ReadUmpPart(buf *bytes.Buffer) (*UmpPart, error) {
	umpType, err := ReadVarInt(buf)
	if err != nil {
		return nil, err
	}

	umpSize, err := ReadVarInt(buf)
	if err != nil {
		return nil, err
	}

	data := make([]byte, umpSize)
	readCount, err := buf.Read(data)
	if err != nil {
		return nil, err
	}

	part := &UmpPart{
		Type: umpType,
		Size: umpSize,
		Data: data[:readCount],
	}

	return part, nil
}

// Treat a UMP buffer as containing a single media fragment of data
// until we encounter a case where that is not true
func ExtractMediaFromUmp(buf *bytes.Buffer) ([]byte, error) {
	var data []byte

	for {
		part, err := ReadUmpPart(buf)

		if part != nil && part.Type == UMP_MEDIA_TYPE {
			data = append(data, part.Data[1:]...)
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return nil, err
		}
	}

	return data, nil
}
