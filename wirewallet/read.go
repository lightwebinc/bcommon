package wirewallet

import (
	"errors"
	"io"
)

// readAtMost reads the whole body up to limit bytes and refuses a longer one.
func readAtMost(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("wallet wire: frame too large")
	}
	return b, nil
}
