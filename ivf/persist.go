package ivf

import (
	"encoding/gob"
	"io"

	"matrixsentry/pq"
)

// snapshot is the gob-serializable form of an Index. Only durable state is
// stored; the byHash/byCode dedup maps are derived and rebuilt on Load.
type snapshot struct {
	Cfg     Config
	Coarse  [][]float32
	PQ      *pq.PQ
	Entries []Handle
	Lists   [][]int32
}

// Save writes the index to w with gob (mirrors pq.Save).
func (ix *Index) Save(w io.Writer) error {
	return gob.NewEncoder(w).Encode(snapshot{
		Cfg:     ix.cfg,
		Coarse:  ix.coarse,
		PQ:      ix.pq,
		Entries: ix.entries,
		Lists:   ix.lists,
	})
}

// Load reconstructs an index written by Save, rebuilding the dedup maps.
func Load(r io.Reader) (*Index, error) {
	var s snapshot
	if err := gob.NewDecoder(r).Decode(&s); err != nil {
		return nil, err
	}
	ix := &Index{
		cfg:     s.Cfg,
		coarse:  s.Coarse,
		pq:      s.PQ,
		entries: s.Entries,
		lists:   s.Lists,
		byHash:  make(map[uint64]int32, len(s.Entries)),
		byCode:  make(map[string]int32, len(s.Entries)),
	}
	for pos, e := range s.Entries {
		ix.byHash[e.Hash] = int32(pos)
		ix.byCode[codeKey(e.Cell, e.Code)] = int32(pos)
	}
	return ix, nil
}
