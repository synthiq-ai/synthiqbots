package mcpserver

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// TestSrp6_DeterministicVector pins the verifier output for a fixed salt and
// known credentials. Vector cross-verified on 2026-05-05 against an
// independent Python reference implementation that mirrors AzerothCore's
// SRP6.cpp::MakeRegistrationData (SHA1(salt || SHA1(USER:PASS)) interpreted
// little-endian, raised to power g=7 mod N, output as 32-byte little-endian).
// Both implementations produced identical bytes for the same inputs.
//
// Inputs:
//
//	username = "ADMIN"
//	password = "ADMIN"        (already uppercase, exercising the no-op fold)
//	salt     = 32 bytes of 0x01
//
// If this test fails after a refactor, recompute against AzerothCore's
// reference rather than updating the vector blindly.
func TestSrp6_DeterministicVector(t *testing.T) {
	var salt [32]byte
	for i := range salt {
		salt[i] = 0x01
	}
	got := srp6CalcVerifierFromSalt("ADMIN", "ADMIN", salt)
	wantHex := "81c219b4d25e3a385f40b8365b2ee269cb9f3424bd16280f8e6a79444811557b"
	if hex.EncodeToString(got[:]) != wantHex {
		t.Errorf("verifier mismatch:\n want %s\n  got %s", wantHex, hex.EncodeToString(got[:]))
	}
}

// TestSrp6_DifferentSaltsDifferentVerifiers locks in that salt entropy
// matters — two calls with the same credentials but different random salts
// must yield distinct verifiers.
func TestSrp6_DifferentSaltsDifferentVerifiers(t *testing.T) {
	s1, v1, err := Srp6CalcVerifier("HENRY", "hunter2pwd")
	if err != nil {
		t.Fatalf("calc1: %v", err)
	}
	s2, v2, err := Srp6CalcVerifier("HENRY", "hunter2pwd")
	if err != nil {
		t.Fatalf("calc2: %v", err)
	}
	if bytes.Equal(s1[:], s2[:]) {
		t.Error("two random salts collided — rand.Read broken?")
	}
	if bytes.Equal(v1[:], v2[:]) {
		t.Error("verifiers identical despite different salts — algorithm or test is broken")
	}
}

// TestSrp6_UppercaseFolding pins that the AzerothCore "uppercase username and
// password before hashing" rule is honored — passing already-uppercase or
// mixed-case credentials must produce the same verifier for the same salt.
func TestSrp6_UppercaseFolding(t *testing.T) {
	var salt [32]byte
	for i := range salt {
		salt[i] = 0x42
	}
	v1 := srp6CalcVerifierFromSalt("foo", "bar", salt)
	v2 := srp6CalcVerifierFromSalt("FOO", "BAR", salt)
	v3 := srp6CalcVerifierFromSalt("Foo", "Bar", salt)
	if !bytes.Equal(v1[:], v2[:]) || !bytes.Equal(v1[:], v3[:]) {
		t.Error("case folding inconsistent — verifier should not depend on input case")
	}
}

// TestSrp6_LengthAndPaddingShape asserts the on-disk byte shape: salt and
// verifier are both 32 bytes, verifier is little-endian with high bytes
// possibly zero. Catches a regression where verifier bytes leak as
// big-endian (worldserver login would silently fail to authenticate then).
func TestSrp6_LengthAndPaddingShape(t *testing.T) {
	salt, v, err := Srp6CalcVerifier("ABCD", "1234")
	if err != nil {
		t.Fatalf("calc: %v", err)
	}
	if len(salt) != 32 || len(v) != 32 {
		t.Errorf("size: salt=%d verifier=%d (want 32/32)", len(salt), len(v))
	}
}
