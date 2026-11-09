//go:build !linux && !darwin && !windows

package localpeer

func platformLookup(...sockKey) (map[sockKey]sockInfo, error) {
	return nil, ErrUnsupported
}
