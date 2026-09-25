"""Authserver (logon) stage of the headless 3.3.5a client.

SRP6 math is pure Python (srp.py). This module only frames the three logon
messages: LOGON_CHALLENGE, LOGON_PROOF, REALM_LIST.
"""
from __future__ import annotations

import socket
import struct
from dataclasses import dataclass

from .srp import SrpClient

CMD_LOGON_CHALLENGE = 0x00
CMD_LOGON_PROOF = 0x01
CMD_REALM_LIST = 0x10

BUILD = 12340  # 3.3.5a


class AuthError(RuntimeError):
    pass


@dataclass
class Realm:
    id: int
    name: str
    address: str  # "host:port"
    flags: int


def _fourcc(s: str) -> bytes:
    """Client sends four-character codes byte-reversed and NUL padded."""
    return s.encode()[::-1].ljust(4, b"\0")


def _recv_exact(sock: socket.socket, n: int) -> bytes:
    buf = bytearray()
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise AuthError(f"authserver closed the connection ({len(buf)}/{n} bytes)")
        buf += chunk
    return bytes(buf)


def logon(host: str, port: int, username: str, password: str, timeout: float = 10.0) -> tuple[bytes, list[Realm]]:
    """Authenticate and fetch the realm list. Returns (session_key, realms).

    The OS tag is "OSX": the core only starts Warden for "Win" clients
    (WorldSession::InitWarden), and this client cannot answer Warden checks.
    """
    username = username.upper()
    with socket.create_connection((host, port), timeout=timeout) as sock:
        user_b = username.encode()
        body = (
            b"WoW\0"
            + bytes([3, 3, 5])
            + struct.pack("<H", BUILD)
            + _fourcc("x86")
            + _fourcc("OSX")
            + _fourcc("enUS")
            + struct.pack("<I", 0)          # timezone bias
            + socket.inet_aton("127.0.0.1")  # client ip (informational)
            + bytes([len(user_b)])
            + user_b
        )
        sock.sendall(bytes([CMD_LOGON_CHALLENGE, 8]) + struct.pack("<H", len(body)) + body)

        cmd, _unk, err = _recv_exact(sock, 3)
        if cmd != CMD_LOGON_CHALLENGE or err != 0:
            raise AuthError(f"logon challenge rejected: cmd={cmd} error={err}")
        server_pub = _recv_exact(sock, 32)
        g_len = _recv_exact(sock, 1)[0]
        g = int.from_bytes(_recv_exact(sock, g_len), "little")
        n_len = _recv_exact(sock, 1)[0]
        n_bytes = _recv_exact(sock, n_len)
        salt = _recv_exact(sock, 32)
        _recv_exact(sock, 16)  # crc salt (version check, unused: StrictVersionCheck=0)
        sec_flags = _recv_exact(sock, 1)[0]
        if sec_flags:
            raise AuthError(f"account has security flags {sec_flags:#x} (PIN/matrix/token) — unsupported")

        srp = SrpClient(username, password, g, n_bytes, server_pub, salt)
        proof = (
            bytes([CMD_LOGON_PROOF])
            + srp.A
            + srp.M1
            + bytes(20)   # crc hash (ignored with StrictVersionCheck=0)
            + bytes([0, 0])  # number of keys, security flags
        )
        sock.sendall(proof)

        cmd, err = _recv_exact(sock, 2)
        if cmd != CMD_LOGON_PROOF or err != 0:
            raise AuthError(f"logon proof rejected: cmd={cmd} error={err} (wrong password or banned)")
        server_proof = _recv_exact(sock, 20)
        _recv_exact(sock, 4 + 4 + 2)  # account flags, survey id, login flags
        if not srp.verify_server(server_proof):
            raise AuthError("server proof did not verify")
        session_key = srp.K

        sock.sendall(bytes([CMD_REALM_LIST]) + struct.pack("<I", 0))
        cmd = _recv_exact(sock, 1)[0]
        if cmd != CMD_REALM_LIST:
            raise AuthError(f"unexpected realm list reply cmd={cmd}")
        (size,) = struct.unpack("<H", _recv_exact(sock, 2))
        data = _recv_exact(sock, size)
        realms = _parse_realms(data)
    return session_key, realms


def _parse_realms(data: bytes) -> list[Realm]:
    off = 4  # unused u32
    (count,) = struct.unpack_from("<H", data, off)
    off += 2
    realms = []
    for _ in range(count):
        _type, _locked, flags = data[off], data[off + 1], data[off + 2]
        off += 3
        end = data.index(b"\0", off)
        name = data[off:end].decode(errors="replace")
        off = end + 1
        end = data.index(b"\0", off)
        address = data[off:end].decode()
        off = end + 1
        off += 4 + 1 + 1  # population float, characters, timezone
        realm_id = data[off]
        off += 1
        if flags & 0x04:
            off += 5  # major, minor, patch, build
        realms.append(Realm(realm_id, name, address, flags))
    return realms
