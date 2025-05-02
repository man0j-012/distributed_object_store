// pkg/erasure/erasure_test.go
package erasure

import (
    "bytes"
    "testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
    enc, err := New(3, 5)
    if err != nil {
        t.Fatalf("New: %v", err)
    }

    input := []byte("The quick brown fox jumps over the lazy dog")
    shards, size, err := enc.Encode(input)
    if err != nil {
        t.Fatalf("Encode: %v", err)
    }

    // Simulate losing two shards
    shards[1] = nil
    shards[4] = nil

    recovered, err := enc.Decode(shards, size)
    if err != nil {
        t.Fatalf("Decode: %v", err)
    }
    if !bytes.Equal(recovered, input) {
        t.Errorf("Recovered mismatch: got %q, want %q", recovered, input)
    }
}
