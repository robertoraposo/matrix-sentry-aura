package sentry

import (
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
)

type Seq uint64
type TenantID uint16
type EventType uint8

const (
	EventAccess EventType = 1
)

type Record struct {
	Seq     Seq
	Tstamp  int64
	Type    EventType
	Tenant  TenantID
	Payload []byte // JSON
}

const HeaderSize = 27 // 4+8+8+1+2+4

// Encode frames a record into a byte slice.
func (r Record) Encode() []byte {
	b := make([]byte, HeaderSize+len(r.Payload))
	binary.LittleEndian.PutUint64(b[4:12], uint64(r.Seq))
	binary.LittleEndian.PutUint64(b[12:20], uint64(r.Tstamp))
	b[20] = byte(r.Type)
	binary.LittleEndian.PutUint16(b[21:23], uint16(r.Tenant))
	binary.LittleEndian.PutUint32(b[23:27], uint32(len(r.Payload)))
	copy(b[27:], r.Payload)

	// CRC32 over everything after the CRC itself.
	crc := crc32.ChecksumIEEE(b[4:])
	binary.LittleEndian.PutUint32(b[0:4], crc)
	return b
}

// DecodeHeader extracts the header and verifies the expected length.
func DecodeHeader(b []byte) (seq Seq, tstamp int64, etype EventType, tenant TenantID, payloadLen uint32, expectedCRC uint32) {
	expectedCRC = binary.LittleEndian.Uint32(b[0:4])
	seq = Seq(binary.LittleEndian.Uint64(b[4:12]))
	tstamp = int64(binary.LittleEndian.Uint64(b[12:20]))
	etype = EventType(b[20])
	tenant = TenantID(binary.LittleEndian.Uint16(b[21:23]))
	payloadLen = binary.LittleEndian.Uint32(b[23:27])
	return
}

type AccessPayload struct {
	ItemID uint64 `json:"item"`
}

func MarshalPayload(v any) ([]byte, error) {
	return json.Marshal(v)
}

func UnmarshalPayload(b []byte, v any) error {
	return json.Unmarshal(b, v)
}
