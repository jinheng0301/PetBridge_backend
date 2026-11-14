package service

import "crypto/rand"

// Alphabet has 32 characters (no 0/O/1/I), matching the table constraint
// ^[2-9A-HJ-NP-Z]{8}$.
const Alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

const codeLen = 8

// NewCode returns a random 8-character code. 32 divides 256, so taking the
// low 5 bits of a random byte is unbiased.
func NewCode() (string, error) {
	b := make([]byte, codeLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = Alphabet[b[i]&31]
	}
	return string(b), nil
}
