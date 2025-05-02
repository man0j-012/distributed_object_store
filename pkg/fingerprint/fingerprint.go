// pkg/fingerprint/fingerprint.go
package fingerprint

import (
  "crypto/rand"
  "encoding/binary"
  "fmt"
)

// Fingerprint holds the secret evaluation point r.
type Fingerprint struct {
  r uint64
}

// NewWithSeed returns a Fingerprint using the provided seed r.
func NewWithSeed(r uint64) *Fingerprint {
  return &Fingerprint{r: r}
}

// Seed returns the evaluation point used by this Fingerprint.
func (f *Fingerprint) Seed() uint64 {
  return f.r
}

// NewRandom generates a secure random non-zero seed for the Fingerprint.
func NewRandom() (*Fingerprint, error) {
  var buf [8]byte
  if _, err := rand.Read(buf[:]); err != nil {
    return nil, fmt.Errorf("failed to read random seed: %w", err)
  }
  r := binary.LittleEndian.Uint64(buf[:])
  if r == 0 {
    r = 1
  }
  return &Fingerprint{r: r}, nil
}

// Eval computes the fingerprint of data by Horner's rule:
//    result = data[0] + data[1]*r + data[2]*r^2 + ...
// using native uint64 overflow as modulo 2^64 arithmetic.
func (f *Fingerprint) Eval(data []byte) uint64 {
  var res uint64
  for _, b := range data {
    res = res*f.r + uint64(b)
  }
  return res
}
