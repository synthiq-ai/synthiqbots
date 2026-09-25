"""WoW (1.12–3.3.5) SRP6 client, pure Python — no compiled dependency.

Mirrors AzerothCore's SRP6.cpp: g=7, k=3, 32-byte little-endian numbers,
x = SHA1(salt | SHA1(UPPER(user):UPPER(pass))), session key K = the
"SHA_Interleave" of S (40 bytes). Proven by a live login on 2026-09-25.
"""
from __future__ import annotations

import hashlib
import secrets

K_MULT = 3


def _le(b: bytes) -> int:
    return int.from_bytes(b, "little")


def _to_le(n: int, size: int = 32) -> bytes:
    return n.to_bytes(size, "little")


def _sha1(*parts: bytes) -> bytes:
    h = hashlib.sha1()
    for p in parts:
        h.update(p)
    return h.digest()


def _interleave(s: bytes) -> bytes:
    """SRP6::SHA1Interleave: drop leading zero bytes (in pairs), hash the even
    and odd halves separately, interleave the two digests."""
    i = 0
    while i < len(s) and s[i] == 0:
        i += 1
    if i & 1:
        i += 1
    s = s[i:]
    even, odd = s[0::2], s[1::2]
    he, ho = _sha1(even), _sha1(odd)
    out = bytearray(40)
    out[0::2], out[1::2] = he, ho
    return bytes(out)


class SrpClient:
    def __init__(self, username: str, password: str, g: int, n_le: bytes, b_le: bytes, salt: bytes):
        user = username.upper().encode()
        N = _le(n_le)
        B = _le(b_le)
        if B % N == 0:
            raise ValueError("server public key B ≡ 0 mod N")
        a = secrets.randbits(19 * 8)
        A = pow(g, a, N)
        self.A = _to_le(A, len(n_le))
        x = _le(_sha1(salt, _sha1(user + b":" + password.upper().encode())))
        u = _le(_sha1(self.A, b_le))
        S = pow((B - K_MULT * pow(g, x, N)) % N, a + u * x, N)
        self.K = _interleave(_to_le(S, len(n_le)))
        hn, hg = _sha1(n_le), _sha1(bytes([g]))
        xor = bytes(p ^ q for p, q in zip(hn, hg))
        self.M1 = _sha1(xor, _sha1(user), salt, self.A, b_le, self.K)
        self._M2 = _sha1(self.A, self.M1, self.K)

    def verify_server(self, m2: bytes) -> bool:
        return m2 == self._M2
