//go:build !linux

package auth

import "errors"

func (keyStore *Store) lock() (func(), error) {
	return nil, errors.New("local key administration currently requires Linux; run it inside WSL")
}
