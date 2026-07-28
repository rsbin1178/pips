package teamintegration

import "crypto/rand"

func cryptographicRead(value []byte) (int, error) {
	return rand.Read(value)
}
