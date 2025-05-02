// pkg/erasure/erasure.go
package erasure

import (
    "bytes"
    "fmt"

    "github.com/klauspost/reedsolomon"
)

// Encoder wraps a Reed-Solomon encoder with parameters for data and total shards.
type Encoder struct {
    re    reedsolomon.Encoder
    data  int
    total int
}

// New creates a Reed-Solomon encoder with 'data' data shards and 'total-data' parity shards.
func New(data, total int) (*Encoder, error) {
    if data <= 0 || total < data {
        return nil, fmt.Errorf("invalid shard parameters: data=%d, total=%d", data, total)
    }
    re, err := reedsolomon.New(data, total-data)
    if err != nil {
        return nil, fmt.Errorf("failed to create RS encoder: %w", err)
    }
    return &Encoder{re: re, data: data, total: total}, nil
}

// Encode splits input into 'total' shards: 'data' data shards and parity shards.
// It returns the shards slice and the original data length, for use in Decode.
func (e *Encoder) Encode(input []byte) ([][]byte, int, error) {
    shards, err := e.re.Split(input)
    if err != nil {
        return nil, 0, fmt.Errorf("split data into shards: %w", err)
    }
    if err = e.re.Encode(shards); err != nil {
        return nil, 0, fmt.Errorf("encode parity shards: %w", err)
    }
    return shards, len(input), nil
}

// Decode reconstructs the original data of length 'outSize' from shards (nil entries allowed).
func (e *Encoder) Decode(shards [][]byte, outSize int) ([]byte, error) {
    if len(shards) != e.total {
        return nil, fmt.Errorf("expected %d shards, got %d", e.total, len(shards))
    }
    if err := e.re.Reconstruct(shards); err != nil {
        return nil, fmt.Errorf("reconstruct shards: %w", err)
    }
    buf := &bytes.Buffer{}
    if err := e.re.Join(buf, shards, outSize); err != nil {
        return nil, fmt.Errorf("join shards: %w", err)
    }
    return buf.Bytes(), nil
}
