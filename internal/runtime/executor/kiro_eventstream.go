// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements an AWS EventStream binary frame parser used by the Kiro executor
// to consume the GenerateAssistantResponse streaming response from AWS CodeWhisperer.
package executor

import (
	"encoding/binary"
	"errors"
)

// kiroEventFrame represents a single AWS EventStream message decoded from the wire.
// Only the fields the Kiro executor needs are surfaced.
type kiroEventFrame struct {
	headers map[string]string
	payload []byte
}

// kiroEventStreamDecoder maintains a sliding byte buffer and yields frames as they
// become available. Callers feed bytes via Append and drain via Next.
type kiroEventStreamDecoder struct {
	buf []byte
}

// Append accumulates more bytes from the upstream response body.
func (d *kiroEventStreamDecoder) Append(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	d.buf = append(d.buf, chunk...)
}

// Next returns the next decoded frame, or (nil, nil) if more bytes are needed.
// Returns an error only for unrecoverable corruption.
func (d *kiroEventStreamDecoder) Next() (*kiroEventFrame, error) {
	if len(d.buf) < 16 {
		return nil, nil
	}
	totalLen := int(binary.BigEndian.Uint32(d.buf[0:4]))
	if totalLen < 16 {
		// Bogus prelude: drop one byte and keep scanning to recover.
		d.buf = d.buf[1:]
		return nil, errors.New("kiro: invalid event frame total length")
	}
	if len(d.buf) < totalLen {
		return nil, nil
	}
	frameBytes := d.buf[:totalLen]
	d.buf = d.buf[totalLen:]

	headersLen := int(binary.BigEndian.Uint32(frameBytes[4:8]))
	if 12+headersLen > totalLen-4 {
		return nil, errors.New("kiro: invalid event frame header length")
	}

	headers := parseKiroHeaders(frameBytes[12 : 12+headersLen])
	payloadStart := 12 + headersLen
	payloadEnd := totalLen - 4 // strip trailing message CRC
	var payload []byte
	if payloadEnd > payloadStart {
		payload = append([]byte(nil), frameBytes[payloadStart:payloadEnd]...)
	}
	return &kiroEventFrame{headers: headers, payload: payload}, nil
}

// parseKiroHeaders decodes the AWS EventStream header block. Only string-typed
// headers (type 7) are surfaced; other types are skipped without error so that
// the parser stays forward-compatible.
func parseKiroHeaders(data []byte) map[string]string {
	headers := make(map[string]string)
	offset := 0
	for offset < len(data) {
		if offset+1 > len(data) {
			return headers
		}
		nameLen := int(data[offset])
		offset++
		if offset+nameLen+1 > len(data) {
			return headers
		}
		name := string(data[offset : offset+nameLen])
		offset += nameLen
		headerType := data[offset]
		offset++
		if headerType != 7 {
			// Unknown / non-string header: bail out, headers we consume are all strings.
			return headers
		}
		if offset+2 > len(data) {
			return headers
		}
		valueLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		offset += 2
		if offset+valueLen > len(data) {
			return headers
		}
		value := string(data[offset : offset+valueLen])
		offset += valueLen
		headers[name] = value
	}
	return headers
}
