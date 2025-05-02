// pkg/fingerprint/fingerprint_test.go
package fingerprint

import (
    "testing"
)

func TestEvalDeterministic(t *testing.T) {
    // Using a fixed seed for reproducible results
    seed := uint64(31)
    fp := NewWithSeed(seed)

    data := []byte{1, 2, 3, 4, 5}
    // Manually compute via Horner:
    // ((((1*r)+2)*r+3)*r+4)*r+5  where r = 31
    want := uint64(986115) // computed by hand

    got := fp.Eval(data)
    if got != want {
        t.Errorf("Eval mismatch: got %d, want %d", got, want)
    }
}

func TestEvalHomomorphic(t *testing.T) {
    // Test Eval(a+b) == Eval(a) + Eval(b) for same-length slices
    seed := uint64(99)
    fp := NewWithSeed(seed)

    a := []byte{10, 20, 30}
    b := []byte{5, 15, 25}
    sum := make([]byte, len(a))
    for i := range a {
        sum[i] = byte((uint16(a[i]) + uint16(b[i])) % 256)
    }

    fa := fp.Eval(a)
    fb := fp.Eval(b)
    fs := fp.Eval(sum)

    if fs != fa+fb {
        t.Errorf("Homomorphic property failed: Eval(sum)=%d, Eval(a)+Eval(b)=%d", fs, fa+fb)
    }
}
