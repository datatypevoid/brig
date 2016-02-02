package store

import (
	"fmt"
	"io"
	"os"
	"sort"
)

// Interval represents a 2D range of integers.
type Interval interface {
	// Range returns the minimum and maximum of the interval.
	// Minimum value is inclusive, maximum value exclusive.
	// In other notation: [min, max)
	Range() (int64, int64)

	// Merge merges the interval `i` to this interval.
	// The range borders should be fixed accordingly,
	// so that [min(i.min, self.min), max(i.max, self.max)] applies.
	Merge(i Interval)
}

// Modification represents a single write
type Modification struct {
	// Offset where the modification started:
	offset int64

	// Data that was changed:
	// This might be changed to a mmap'd byte slice later.
	data []byte
}

// Range returns the fitting integer interval
func (n *Modification) Range() (int64, int64) {
	return n.offset, n.offset + int64(len(n.data))
}

// Merge adds the data of another interval where they intersect.
// The overlapping parts are taken from `n` always.
// Note: `i` shall not be used after calling Merge.
func (n *Modification) Merge(i Interval) {
	// Interracial merges are forbidden :-(
	other, ok := i.(*Modification)
	if !ok {
		return
	}

	oMin, oMax := other.Range()
	nMin, nMax := n.Range()

	// Check if the intervals overlap.
	// If not, there's nothing left to do.
	if nMin > oMax || oMin > nMax {
		return
	}

	// Prepend non-overlapping data from `other`:
	if nMin > oMin {
		n.data = append(other.data[:(nMin-oMin)], n.data...)
		n.offset = other.offset
	}

	// Append non-overlapping data from `other`:
	if nMax < oMax {
		// Append other.data[(other.Max - n.Max):]
		n.data = append(n.data, other.data[(oMax-nMax-1):]...)
	}

	// Make sure old data gets invalidated quickly:
	other.data = nil
}

// IntervalIndex represents a continous array of sorted intervals.
// When adding intervals to the index, it will merge them overlapping areas.
// Holes between the intervals are allowed.
type IntervalIndex struct {
	r []Interval

	// Max is the maximum interval offset given to Add()
	Max int64
}

// cut deletes the a[i:j] from a and returns the new slice.
func cut(a []Interval, i, j int) []Interval {
	copy(a[i:], a[j:])
	for k, n := len(a)-j+i, len(a); k < n; k++ {
		a[k] = nil
	}
	return a[:len(a)-j+i]
}

// insert squeezes `x` at a[j] and moves the reminding elements.
// Returns the modified slice.
func insert(a []Interval, i int, x Interval) []Interval {
	a = append(a, nil)
	copy(a[i+1:], a[i:])
	a[i] = x
	return a
}

// Add inserts a single interval to the index.
// If it overlaps with existing intervals, it's data will
// take priority over other intervals.
func (ivl *IntervalIndex) Add(n Interval) {
	Min, Max := n.Range()
	if Max < Min {
		panic("Max > Min!")
	}

	// Initial case: Add as single element.
	if ivl.r == nil {
		ivl.r = []Interval{n}
		ivl.Max = Max
		return
	}

	// Find the lowest fitting interval:
	minIdx := sort.Search(len(ivl.r), func(i int) bool {
		_, iMax := ivl.r[i].Range()
		return Min <= iMax
	})

	// Find the highest fitting interval:
	maxIdx := sort.Search(len(ivl.r), func(i int) bool {
		iMin, _ := ivl.r[i].Range()
		return Max <= iMin
	})

	// Remember biggest offset:
	if Max > ivl.Max {
		ivl.Max = Max
	}

	// New interval is bigger than all others:
	if minIdx >= len(ivl.r) {
		ivl.r = append(ivl.r, n)
		return
	}

	// New range fits nicely in; just insert it in between:
	if minIdx == maxIdx {
		ivl.r = insert(ivl.r, minIdx, n)
		return
	}

	// Something in between. Merge to continuous interval:
	for i := minIdx; i < maxIdx; i++ {
		n.Merge(ivl.r[i])
	}

	// Delete old unmerged intervals and substitute with merged:
	ivl.r[minIdx] = n
	ivl.r = cut(ivl.r, minIdx+1, maxIdx)
}

// Overlays returns all intervals that intersect with [start, end)
func (ivl *IntervalIndex) Overlays(start, end int64) []Interval {
	// Find the lowest matching interval:
	lo := sort.Search(len(ivl.r), func(i int) bool {
		_, iMax := ivl.r[i].Range()
		return start <= iMax
	})

	hi := sort.Search(len(ivl.r), func(i int) bool {
		iMin, _ := ivl.r[i].Range()
		return end <= iMin
	})

	return ivl.r[lo:hi]
}

// min returns the minimum of a and b.
func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// max returns the maximum of a and b.
func max(a, b int64) int64 {
	if a < b {
		return b
	}
	return a
}

// Layer is a io.ReadWriter that takes an underlying Reader
// and caches Writes on top of it. To the outside it delivers
// a zipped stream of the recent writes and the underlying stream.
type Layer struct {
	index *IntervalIndex
	r     io.ReadSeeker
	pos   int64
	limit int64
}

// NewLayer returns a new in memory layer.
// No IO is performed on creation.
func NewLayer(r io.ReadSeeker) *Layer {
	return &Layer{
		index: &IntervalIndex{},
		r:     r,
		limit: -1,
	}
}

// Write caches the buffer in memory or on disk until the file is closed.
// If the file was truncated before, the truncate limit is raised again
// if the write extended the limit.
func (l *Layer) Write(buf []byte) (int, error) {
	l.index.Add(&Modification{l.pos, buf})
	l.pos += int64(len(buf))
	if l.limit >= 0 && l.pos > l.limit {
		l.limit = l.pos
	}

	return len(buf), nil
}

// Read will read from the underlying stream and overlay with the relevant
// write chunks on it's way, possibly extending the underlying stream.
func (l *Layer) Read(buf []byte) (int, error) {
	// Check for the truncation limit:
	if l.limit >= 0 {
		truncateDiff := l.limit - l.pos

		// Seems we're over the limit:
		if truncateDiff <= 0 {
			return 0, io.EOF
		}

		// Truncate buf, so we don't read too much:
		if truncateDiff < int64(len(buf)) {
			buf = buf[:truncateDiff]
		}
	}

	n, err := l.r.Read(buf)
	if err == io.EOF && l.pos < l.index.Max {
		// There's only extending writes left.
		// Empty `buf` so caller get's defined results.
		// This should not happen in practice, but helps identifying bugs.
		for i := n; i < len(buf); i++ {
			// TODO: Turn this back to a nul-byte.
			//       @ just is nice for debugging.
			buf[i] = byte('@')
		}

		// Forget about EOF for a short moment.
		err = nil
	}

	// Check for other errors:
	if err != nil {
		return 0, err
	}

	// Check which write chunks are overlaying this buf:
	for _, chunk := range l.index.Overlays(l.pos, l.pos+int64(len(buf))) {
		// Tip: Drawing this on paper helps to understand the following.
		mod := chunk.(*Modification)

		// Overlapping area in absolute offsets:
		lo, hi := mod.Range()
		a, b := max(lo, l.pos), min(hi, l.pos+int64(len(buf)))

		// Convert to relative offsets:
		overlap, chunkLo, bufLo := int(b-a), int(a-lo), int(a-l.pos)

		// Copy overlapping data:
		copy(buf[bufLo:bufLo+overlap], mod.data[chunkLo:chunkLo+overlap])

		// Extend, if write chunks go over original data stream:
		// (caller wants max. offset where we wrote data to buf)
		if bufLo+overlap > n {
			n = bufLo + overlap
		}
	}

	l.pos += int64(n)
	return n, nil
}

// Seek remembers the new position and delegates the seek down.
// Note: if the file was truncated before, a seek after the limit
//       will extend the truncation again and NOT return io.EOF.
//       This might be surprising, but is convenient for first
//       truncating to zero and then writing to the file.
func (l *Layer) Seek(offset int64, whence int) (int64, error) {
	newPos := l.pos

	switch whence {
	case os.SEEK_CUR:
		newPos += offset
	case os.SEEK_SET:
		newPos = offset
	case os.SEEK_END:
		return 0, fmt.Errorf("layer: SEEK_END not supported")
	}

	// Check if we hit the truncate limit:
	if l.limit >= 0 && l.limit < newPos {
		l.limit = newPos
	}

	l.pos = newPos

	return l.r.Seek(offset, whence)
}

// Close tries to close the underlying stream (if supported).
func (l *Layer) Close() error {
	if closer, ok := l.r.(io.Closer); ok {
		return closer.Close()
	}

	return nil
}

// Truncate cuts off the stream at `size` bytes.
// After hitting the limit, io.EOF is returned.
// A value < 0 disables truncation.
func (l *Layer) Truncate(size int64) {
	l.limit = size
}

// Limit returns the current truncation limit
// or a number < 0 if no truncation is done.
func (l *Layer) Limit() int64 {
	return l.limit
}

// Size returns the minimum size that the layer will have.
// Underlying stream might be larger, so caller needs to check that.
func (l *Layer) MinSize() int64 {
	if l.limit < 0 || l.index.Max < l.limit {
		return int64(l.index.Max)
	}

	return int64(l.limit)
}