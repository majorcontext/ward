package mirror

// planChunks splits n commits into pushes. It returns the index of the last commit of each push, in order. fits(from, to)
// says whether the objects of commit to that commit from does not reach (from -1 means none) fit in one push. A commit
// that does not fit alone is still a push of its own.
func planChunks(n int, fits func(from, to int) bool) []int {
	var out []int
	for from := -1; from < n-1; {
		lo, hi := from+1, n-1
		if fits(from, hi) {
			lo = hi
		}
		for lo < hi {
			mid := lo + (hi-lo+1)/2
			if fits(from, mid) {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		out = append(out, lo)
		from = lo
	}
	return out
}
