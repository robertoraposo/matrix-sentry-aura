package ivf

import (
	"sort"

	"matrixsentry/pq"
)

// maxHeap keeps the topK smallest distances using a bounded max-heap (largest at
// the root), so a full scan of a cell costs O(n log k) instead of O(n log n).
type maxHeap []pq.Result

func (h maxHeap) Len() int { return len(h) }

func (h *maxHeap) push(r pq.Result) {
	*h = append(*h, r)
	i := len(*h) - 1
	for i > 0 {
		p := (i - 1) / 2
		if (*h)[p].Dist >= (*h)[i].Dist {
			break
		}
		(*h)[p], (*h)[i] = (*h)[i], (*h)[p]
		i = p
	}
}

func (h *maxHeap) replaceTop(r pq.Result) {
	(*h)[0] = r
	n, i := len(*h), 0
	for {
		l, ri, largest := 2*i+1, 2*i+2, i
		if l < n && (*h)[l].Dist > (*h)[largest].Dist {
			largest = l
		}
		if ri < n && (*h)[ri].Dist > (*h)[largest].Dist {
			largest = ri
		}
		if largest == i {
			break
		}
		(*h)[i], (*h)[largest] = (*h)[largest], (*h)[i]
		i = largest
	}
}

// drainAscending returns the heap contents sorted by increasing distance, with
// ties broken by ID so the result order is deterministic.
func (h *maxHeap) drainAscending() []pq.Result {
	out := make([]pq.Result, len(*h))
	copy(out, *h)
	sort.Slice(out, func(a, b int) bool {
		if out[a].Dist != out[b].Dist {
			return out[a].Dist < out[b].Dist
		}
		return out[a].ID < out[b].ID
	})
	return out
}
