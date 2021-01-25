package util

// CompareStringSlices returns true if two string slices are of the same length
// and have the values in the same order. If the order does not matter for your
// comparison, call after sorting the slices.
func CompareStringSlices(a []string, b []string) bool {

	if a == nil && b == nil {
		return true
	}

	if !(a != nil && b != nil) {
		return false
	}

	if len(a) != len(b) {
		return false
	}

	for index, value := range a {
		if value != b[index] {
			return false
		}
	}

	return true
}
