package sentry

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var (
	ErrCorrupt = errors.New("sentry: journal corruption detected")
)

type location struct {
	seg  uint32
	off  int64
	size uint32
}

type Store struct {
	dir     string
	opt     Options
	mu      sync.Mutex
	cur     *os.File
	curSeg  uint32
	curOff  int64
	nextSeq Seq
	keydir  []location
}

type Options struct {
	SegmentMax int64
	FsyncEvery time.Duration
}

func Open(dir string, opt Options) (*Store, error) {
	if opt.SegmentMax == 0 {
		opt.SegmentMax = 256 << 20
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	s := &Store{
		dir:     dir,
		opt:     opt,
		nextSeq: 1,
	}

	if err := s.recover(); err != nil {
		return nil, err
	}

	if err := s.openSegment(s.curSeg); err != nil {
		return nil, err
	}
	off, err := s.cur.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	s.curOff = off

	return s, nil
}

func (s *Store) recover() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}

	var segs []uint32
	for _, e := range entries {
		var seg uint32
		if n, _ := fmt.Sscanf(e.Name(), "%06d.log", &seg); n == 1 {
			segs = append(segs, seg)
		}
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i] < segs[j] })

	for i, seg := range segs {
		path := filepath.Join(s.dir, fmt.Sprintf("%06d.log", seg))
		f, err := os.Open(path)
		if err != nil {
			return err
		}

		off := int64(0)
		for {
			header := make([]byte, HeaderSize)
			_, err := io.ReadFull(f, header)
			if err == io.EOF {
				break
			}
			if err != nil {
				if i == len(segs)-1 {
					f.Close()
					if err := os.Truncate(path, off); err != nil {
						return err
					}
					break
				}
				f.Close()
				return fmt.Errorf("%w at segment %d, offset %d: %v", ErrCorrupt, seg, off, err)
			}

			_, _, _, _, payloadLen, expectedCRC := DecodeHeader(header)
			payload := make([]byte, payloadLen)
			if _, err := io.ReadFull(f, payload); err != nil {
				if i == len(segs)-1 {
					f.Close()
					if err := os.Truncate(path, off); err != nil {
						return err
					}
					break
				}
				f.Close()
				return fmt.Errorf("%w at segment %d, offset %d: truncated payload", ErrCorrupt, seg, off)
			}

			actualCRC := crc32.ChecksumIEEE(header[4:])
			actualCRC = crc32.Update(actualCRC, crc32.IEEETable, payload)
			if actualCRC != expectedCRC {
				if i == len(segs)-1 {
					f.Close()
					if err := os.Truncate(path, off); err != nil {
						return err
					}
					break
				}
				f.Close()
				return fmt.Errorf("%w at segment %d, offset %d: crc mismatch", ErrCorrupt, seg, off)
			}

			s.keydir = append(s.keydir, location{seg: seg, off: off, size: uint32(HeaderSize + payloadLen)})
			s.nextSeq++
			off += int64(HeaderSize + payloadLen)
		}
		f.Close()
		s.curSeg = seg
	}

	return nil
}

func (s *Store) openSegment(seg uint32) error {
	if s.cur != nil {
		s.cur.Close()
	}
	path := filepath.Join(s.dir, fmt.Sprintf("%06d.log", seg))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	s.cur = f
	s.curSeg = seg
	return nil
}

func (s *Store) Append(tenant TenantID, t EventType, payload any) (Seq, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	jsonPayload, err := MarshalPayload(payload)
	if err != nil {
		return 0, err
	}

	rec := Record{
		Seq:     s.nextSeq,
		Tstamp:  time.Now().UnixNano(),
		Type:    t,
		Tenant:  tenant,
		Payload: jsonPayload,
	}

	b := rec.Encode()

	if s.curOff+int64(len(b)) > s.opt.SegmentMax {
		if err := s.openSegment(s.curSeg + 1); err != nil {
			return 0, err
		}
		s.curOff = 0
	}

	if _, err := s.cur.Write(b); err != nil {
		return 0, err
	}

	if s.opt.FsyncEvery == 0 {
		if err := s.cur.Sync(); err != nil {
			return 0, err
		}
	}

	s.keydir = append(s.keydir, location{seg: s.curSeg, off: s.curOff, size: uint32(len(b))})
	seq := s.nextSeq
	s.nextSeq++
	s.curOff += int64(len(b))

	return seq, nil
}

func (s *Store) Read(seq Seq) (Record, error) {
	if seq == 0 || int(seq) > len(s.keydir) {
		return Record{}, fmt.Errorf("seq %d not found", seq)
	}
	loc := s.keydir[seq-1]

	path := filepath.Join(s.dir, fmt.Sprintf("%06d.log", loc.seg))
	f, err := os.Open(path)
	if err != nil {
		return Record{}, err
	}
	defer f.Close()

	b := make([]byte, loc.size)
	if _, err := f.ReadAt(b, loc.off); err != nil {
		return Record{}, err
	}

	_, tstamp, etype, tenant, _, expectedCRC := DecodeHeader(b[:HeaderSize])
	payload := b[HeaderSize:]

	actualCRC := crc32.ChecksumIEEE(b[4:HeaderSize])
	actualCRC = crc32.Update(actualCRC, crc32.IEEETable, payload)
	if actualCRC != expectedCRC {
		return Record{}, ErrCorrupt
	}

	return Record{
		Seq:     seq,
		Tstamp:  tstamp,
		Type:    etype,
		Tenant:  tenant,
		Payload: payload,
	}, nil
}

func (s *Store) ReadNextSeq() Seq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextSeq
}

type Filter struct {
	Tenant *TenantID
	Type   *EventType
}

func (s *Store) Scan(filter Filter, fn func(Record) bool) error {
	for i := range s.keydir {
		rec, err := s.Read(Seq(i + 1))
		if err != nil {
			return err
		}
		if filter.Tenant != nil && rec.Tenant != *filter.Tenant {
			continue
		}
		if filter.Type != nil && rec.Type != *filter.Type {
			continue
		}
		if !fn(rec) {
			break
		}
	}
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil {
		if err := s.cur.Sync(); err != nil {
			return err
		}
		return s.cur.Close()
	}
	return nil
}
