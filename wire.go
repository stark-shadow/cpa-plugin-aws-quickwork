package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
)

const (
	hvBoolTrue  = 0
	hvBoolFalse = 1
	hvByte      = 2
	hvShort     = 3
	hvInt       = 4
	hvLong      = 5
	hvBytes     = 6
	hvString    = 7
	hvTimestamp = 8
	hvUUID      = 9
)

type wireError struct {
	Code    string
	Message string
}

func (e *wireError) Error() string {
	if e.Code != "" && e.Message != "" {
		return fmt.Sprintf("upstream error [%s] %s", e.Code, e.Message)
	}
	if e.Code != "" {
		return fmt.Sprintf("upstream error [%s]", e.Code)
	}
	if e.Message != "" {
		return "upstream error: " + e.Message
	}
	return "upstream event exception"
}

func decodeEventHeaders(b []byte) (map[string]any, error) {
	out := make(map[string]any)
	i := 0
	for i < len(b) {
		nameLen := int(b[i])
		i++
		if i+nameLen > len(b) {
			return nil, errors.New("truncated event header name")
		}
		name := string(b[i : i+nameLen])
		i += nameLen
		if i >= len(b) {
			return nil, errors.New("truncated event header type")
		}
		typ := b[i]
		i++
		switch typ {
		case hvBoolTrue:
			out[name] = true
		case hvBoolFalse:
			out[name] = false
		case hvByte:
			if i+1 > len(b) {
				return nil, errors.New("truncated byte header")
			}
			out[name] = b[i]
			i++
		case hvShort:
			if i+2 > len(b) {
				return nil, errors.New("truncated short header")
			}
			out[name] = int(binary.BigEndian.Uint16(b[i : i+2]))
			i += 2
		case hvInt:
			if i+4 > len(b) {
				return nil, errors.New("truncated int header")
			}
			out[name] = binary.BigEndian.Uint32(b[i : i+4])
			i += 4
		case hvLong, hvTimestamp:
			if i+8 > len(b) {
				return nil, errors.New("truncated long header")
			}
			out[name] = binary.BigEndian.Uint64(b[i : i+8])
			i += 8
		case hvBytes:
			if i+2 > len(b) {
				return nil, errors.New("truncated bytes header")
			}
			n := int(binary.BigEndian.Uint16(b[i : i+2]))
			i += 2
			if i+n > len(b) {
				return nil, errors.New("truncated bytes header value")
			}
			out[name] = append([]byte(nil), b[i:i+n]...)
			i += n
		case hvString:
			if i+2 > len(b) {
				return nil, errors.New("truncated string header")
			}
			n := int(binary.BigEndian.Uint16(b[i : i+2]))
			i += 2
			if i+n > len(b) {
				return nil, errors.New("truncated string header value")
			}
			out[name] = string(b[i : i+n])
			i += n
		case hvUUID:
			if i+16 > len(b) {
				return nil, errors.New("truncated uuid header")
			}
			out[name] = append([]byte(nil), b[i:i+16]...)
			i += 16
		default:
			return nil, fmt.Errorf("unsupported event header type %d", typ)
		}
	}
	return out, nil
}

func headerString(headers map[string]any, key string) string {
	switch v := headers[key].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}

// aidpOutput decodes either a plain JSON frame or one or more AWS event-stream
// messages packed into a single WebSocket binary frame, verifying both CRCs.
func aidpOutput(frame []byte) ([][]byte, error) {
	if json.Valid(frame) {
		var wrapper struct{ Type, Payload string }
		if json.Unmarshal(frame, &wrapper) == nil && wrapper.Type == "" && wrapper.Payload != "" {
			decoded, err := base64.StdEncoding.DecodeString(wrapper.Payload)
			return [][]byte{decoded}, err
		}
		return [][]byte{frame}, nil
	}
	var result [][]byte
	var firstErr error
	for len(frame) > 0 {
		if len(frame) < 16 {
			if len(result) > 0 {
				return result, io.ErrUnexpectedEOF
			}
			return nil, io.ErrUnexpectedEOF
		}
		size := int(binary.BigEndian.Uint32(frame[0:4]))
		headersLen := int(binary.BigEndian.Uint32(frame[4:8]))
		if size < 16 || size > len(frame) || headersLen > size-16 ||
			crc32.ChecksumIEEE(frame[:8]) != binary.BigEndian.Uint32(frame[8:12]) ||
			crc32.ChecksumIEEE(frame[:size-4]) != binary.BigEndian.Uint32(frame[size-4:size]) {
			return nil, errors.New("invalid event frame")
		}
		headers, err := decodeEventHeaders(frame[12 : 12+headersLen])
		if err != nil {
			return nil, errors.New("invalid event headers")
		}
		payload := frame[12+headersLen : size-4]
		mt := headerString(headers, ":message-type")
		errCode := headerString(headers, ":error-code")
		excType := headerString(headers, ":exception-type")
		if mt == "error" || mt == "exception" || errCode != "" || excType != "" {
			code := errCode
			if code == "" {
				code = excType
			}
			detail := strings.TrimSpace(headerString(headers, ":error-message"))
			if detail == "" && len(payload) > 0 {
				detail = errorText(payload, "", code)
			}
			if firstErr == nil {
				firstErr = &wireError{Code: code, Message: detail}
			}
			frame = frame[size:]
			continue
		}
		var wrapper struct{ Payload string }
		if json.Unmarshal(payload, &wrapper) != nil || wrapper.Payload == "" {
			if firstErr == nil {
				firstErr = errors.New("invalid event payload")
			}
			frame = frame[size:]
			continue
		}
		data, err := base64.StdEncoding.DecodeString(wrapper.Payload)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			frame = frame[size:]
			continue
		}
		result = append(result, data)
		frame = frame[size:]
	}
	return result, firstErr
}
