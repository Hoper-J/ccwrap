//go:build !linux && !darwin

package localpeer

func platformLookup(...sockKey) (map[sockKey]sockInfo, error) {
	return nil, ErrUnsupported
}
