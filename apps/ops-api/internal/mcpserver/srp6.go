package mcpserver

import (
	"crypto/rand"
	"crypto/sha1"
	"fmt"
	"math/big"
	"strings"
)

// AzerothCore SRP6 verifier computation. Mirrors src/common/Cryptography/SRP6.cpp:
//
//	v = g^H(salt || H(uppercase(username) || ':' || uppercase(password))) mod N
//
// Both username and password are uppercased before hashing — this matches
// AccountMgr::CreateAccount's Utf8ToUpperOnlyLatin step. Salt is 32 random
// bytes; verifier is the 32-byte little-endian representation of v (zero-padded
// at the high end).
//
// Used by wow_create_account to INSERT INTO acore_auth.account (salt, verifier).
// The original wow-admin tools tried to shell into a non-existent
// `worldserver-cli`; this SRP6 path lets us create accounts via direct SQL
// against ops_rw without any console-attach gymnastics on ac-worldserver.

const srp6NHex = "894B645E89E1535BBDAD5B8B290650530801B18EBFBF5E8FAB3C82872A3E9BB7"

var (
	srp6N = mustParseHex(srp6NHex)
	srp6G = big.NewInt(7)
)

func mustParseHex(h string) *big.Int {
	n, ok := new(big.Int).SetString(h, 16)
	if !ok {
		panic("srp6: bad hex constant " + h)
	}
	return n
}

// Srp6CalcVerifier returns the 32-byte salt + 32-byte verifier for an
// AzerothCore account. Salt is generated fresh on each call.
func Srp6CalcVerifier(username, password string) (salt [32]byte, verifier [32]byte, err error) {
	if _, err = rand.Read(salt[:]); err != nil {
		return salt, verifier, fmt.Errorf("rand salt: %w", err)
	}
	verifier = srp6CalcVerifierFromSalt(username, password, salt)
	return salt, verifier, nil
}

// srp6CalcVerifierFromSalt is the pure deterministic core, factored out so
// tests can pin a salt and assert the verifier byte-for-byte.
func srp6CalcVerifierFromSalt(username, password string, salt [32]byte) (verifier [32]byte) {
	user := strings.ToUpper(username)
	pass := strings.ToUpper(password)

	h1 := sha1.Sum([]byte(user + ":" + pass))

	h2 := sha1.New()
	h2.Write(salt[:])
	h2.Write(h1[:])
	xLE := h2.Sum(nil)

	// xLE is the SHA1 digest interpreted as a little-endian integer (matches
	// AzerothCore BigNumber::SetBinary which reads byte 0 as the low byte).
	x := new(big.Int).SetBytes(reverseBytes(xLE))

	v := new(big.Int).Exp(srp6G, x, srp6N)

	// AzerothCore stores verifier as little-endian, zero-padded to 32 bytes.
	be := v.Bytes()
	for i, b := range be {
		verifier[len(be)-1-i] = b
	}
	return verifier
}

func reverseBytes(b []byte) []byte {
	out := make([]byte, len(b))
	for i, x := range b {
		out[len(b)-1-i] = x
	}
	return out
}
