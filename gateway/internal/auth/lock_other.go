//go:build !linux

package auth

import "errors"

func (s *Store) lock() (func(), error) {
	return nil, errors.New("local key administration currently requires Linux; run it inside WSL")
}
