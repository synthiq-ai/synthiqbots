"""World-server stage of the headless 3.3.5a client (build 12340).

A real, non-bot WorldSession: to the core and to mod-playerbots this is a
human at a keyboard, which is exactly what IsWhitelistedHumanPlayer needs.
It never moves on its own and never fights — scenarios move it with GM
teleports and it acknowledges them like a real client must.

Every opcode and layout here was checked against the deployed core
(Opcodes.h, WorldSocket.cpp, Chat.cpp, ChatHandler.cpp) on 2026-09-25.
"""
from __future__ import annotations

import hashlib
import hmac
import os
import queue
import socket
import struct
import threading
import time
import zlib
from dataclasses import dataclass, field

from .auth import BUILD

# ---- opcodes (verified against src/server/game/Server/Protocol/Opcodes.h) ----
SMSG_AUTH_CHALLENGE = 0x1EC
CMSG_AUTH_SESSION = 0x1ED
SMSG_AUTH_RESPONSE = 0x1EE
CMSG_CHAR_ENUM = 0x037
SMSG_CHAR_ENUM = 0x03B
CMSG_CHAR_CREATE = 0x036
SMSG_CHAR_CREATE = 0x03A
CMSG_PLAYER_LOGIN = 0x03D
SMSG_LOGIN_VERIFY_WORLD = 0x236
CMSG_MESSAGECHAT = 0x095
SMSG_MESSAGECHAT = 0x096
SMSG_GM_MESSAGECHAT = 0x3B3
CMSG_NAME_QUERY = 0x050
SMSG_NAME_QUERY_RESPONSE = 0x051
SMSG_TIME_SYNC_REQ = 0x390
CMSG_TIME_SYNC_RESP = 0x391
CMSG_READY_FOR_ACCOUNT_DATA_TIMES = 0x4FF
CMSG_SET_ACTIVE_MOVER = 0x26A
CMSG_KEEP_ALIVE = 0x407
CMSG_PING = 0x1DC
MSG_MOVE_TELEPORT_ACK = 0x0C7
MSG_MOVE_WORLDPORT_ACK = 0x0DC
SMSG_NEW_WORLD = 0x03E
CMSG_GROUP_INVITE = 0x06E
SMSG_GROUP_INVITE = 0x06F
CMSG_GROUP_ACCEPT = 0x072
CMSG_GROUP_DECLINE = 0x073
CMSG_GROUP_DISBAND = 0x07B
SMSG_GROUP_LIST = 0x07D
SMSG_PARTY_COMMAND_RESULT = 0x07F
SMSG_GROUP_DESTROYED = 0x07C
SMSG_GROUP_UNINVITE = 0x077
CMSG_SET_SELECTION = 0x13D
CMSG_LOGOUT_REQUEST = 0x04B
SMSG_LOGOUT_COMPLETE = 0x04D
SMSG_WARDEN_DATA = 0x2E6

AUTH_OK = 0x0C
CHAR_CREATE_SUCCESS = 0x2F

# chat types (SharedDefines.h)
CHAT_SYSTEM, CHAT_SAY, CHAT_PARTY, CHAT_WHISPER, CHAT_WHISPER_INFORM = 0x00, 0x01, 0x02, 0x07, 0x09
CHAT_PARTY_LEADER = 0x33
# ChatHandler::BuildChatPacket layout groups (enum values from the deployed SharedDefines.h)
_NAMED_SENDER = {0x0C, 0x0D, 0x0E, 0x0F, 0x10, 0x29, 0x2A, 0x2F}  # monster say/party/yell/whisper/emote, boss emote/whisper, battlenet
_WHISPER_FOREIGN = 0x08
_BG_SYSTEM = {0x24, 0x25, 0x26}
_ACHIEVEMENT = {0x30, 0x31}
_CHANNEL = 0x11
LANG_ORCISH = 1  # Horde player language; LANG_UNIVERSAL is refused as a cheat


class WorldError(RuntimeError):
    pass


@dataclass
class Character:
    guid: int
    name: str
    race: int
    cls: int
    level: int
    map: int
    zone: int


@dataclass
class ChatEvent:
    ts: float
    chat_type: int
    sender_guid: int
    sender_name: str
    text: str
    gm: bool = False


@dataclass
class Event:
    ts: float
    kind: str
    data: dict = field(default_factory=dict)


# Header cipher keys, AuthCrypt.cpp: the server ENCRYPTS with HMAC(ServerEncryptionKey, K)
# and DECRYPTS client headers with HMAC(ServerDecryptionKey, K); ARC4, first 1024 bytes dropped.
_SERVER_ENCRYPTION_KEY = bytes.fromhex("CC98AE04E897EACA12DDC09342915357")
_SERVER_DECRYPTION_KEY = bytes.fromhex("C2B3723CC6AED9B5343C53EE2F4367CE")


class _Rc4:
    """Byte-at-a-time ARC4 — headers only, so speed does not matter, and
    byte granularity lets us decrypt the first header byte before knowing
    whether the header is 4 or 5 bytes long."""

    def __init__(self, key: bytes, drop: int = 1024):
        S = list(range(256))
        j = 0
        for i in range(256):
            j = (j + S[i] + key[i % len(key)]) & 0xFF
            S[i], S[j] = S[j], S[i]
        self.S, self.i, self.j = S, 0, 0
        self.apply(bytes(drop))

    def apply(self, data: bytes) -> bytes:
        S, i, j = self.S, self.i, self.j
        out = bytearray(len(data))
        for n, c in enumerate(data):
            i = (i + 1) & 0xFF
            j = (j + S[i]) & 0xFF
            S[i], S[j] = S[j], S[i]
            out[n] = c ^ S[(S[i] + S[j]) & 0xFF]
        self.i, self.j = i, j
        return bytes(out)


class _Buf:
    def __init__(self, data: bytes):
        self.d, self.o = data, 0

    def u8(self):  v = self.d[self.o]; self.o += 1; return v
    def u16(self): v = struct.unpack_from("<H", self.d, self.o)[0]; self.o += 2; return v
    def u32(self): v = struct.unpack_from("<I", self.d, self.o)[0]; self.o += 4; return v
    def i32(self): v = struct.unpack_from("<i", self.d, self.o)[0]; self.o += 4; return v
    def u64(self): v = struct.unpack_from("<Q", self.d, self.o)[0]; self.o += 8; return v
    def f32(self): v = struct.unpack_from("<f", self.d, self.o)[0]; self.o += 4; return v

    def cstr(self):
        end = self.d.index(b"\0", self.o)
        v = self.d[self.o:end].decode(errors="replace")
        self.o = end + 1
        return v

    def packed_guid(self):
        mask = self.u8()
        g = 0
        for i in range(8):
            if mask & (1 << i):
                g |= self.u8() << (8 * i)
        return g

    def skip(self, n): self.o += n


def _packed_guid(g: int) -> bytes:
    mask, out = 0, bytearray()
    for i in range(8):
        b = (g >> (8 * i)) & 0xFF
        if b:
            mask |= 1 << i
            out.append(b)
    return bytes([mask]) + bytes(out)


class WorldClient:
    """One logged-in character. Thread-safe send; a background thread reads."""

    def __init__(self, host: str, port: int, username: str, session_key: bytes, realm_id: int = 1,
                 log=print):
        self.username = username.upper()
        self.session_key = session_key
        self.realm_id = realm_id
        self.log = log
        self.sock = socket.create_connection((host, port), timeout=15)
        self._rx_cipher = None
        self._tx_cipher = None
        self._send_lock = threading.Lock()
        self._rx = bytearray()
        self._inbox: "queue.Queue[tuple[int, bytes]]" = queue.Queue()
        self.chat: list[ChatEvent] = []
        self.events: list[Event] = []
        self.names: dict[int, str] = {}
        self.me: Character | None = None
        self.in_world = False
        self._t0 = time.monotonic()
        self._stop = threading.Event()
        self._lock = threading.Lock()
        try:
            self._handshake()          # still under the 15 s socket timeout (codex)
        except Exception:
            self.sock.close()
            raise
        self.sock.settimeout(None)     # the reader thread blocks from here on
        self._reader = threading.Thread(target=self._read_loop, name="wow-rx", daemon=True)
        self._reader.start()
        self._ticker = threading.Thread(target=self._keepalive_loop, name="wow-ka", daemon=True)
        self._ticker.start()

    # ---- framing -------------------------------------------------------
    def _recv_exact(self, n: int) -> bytes:
        while len(self._rx) < n:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise WorldError("worldserver closed the connection")
            self._rx += chunk
        out = bytes(self._rx[:n])
        del self._rx[:n]
        return out

    def _read_packet(self) -> tuple[int, bytes]:
        if self._rx_cipher is None:
            head = self._recv_exact(4)
            size, opcode = struct.unpack(">H", head[:2])[0], struct.unpack("<H", head[2:])[0]
        else:
            first = self._rx_cipher.apply(self._recv_exact(1))
            if first[0] & 0x80:   # large packet: 3-byte size
                rest = self._rx_cipher.apply(self._recv_exact(4))
                size = ((first[0] & 0x7F) << 16) | (rest[0] << 8) | rest[1]
                opcode = struct.unpack("<H", rest[2:4])[0]
            else:
                rest = self._rx_cipher.apply(self._recv_exact(3))
                size = (first[0] << 8) | rest[0]
                opcode = struct.unpack("<H", rest[1:3])[0]
        body = self._recv_exact(size - 2)
        return opcode, body

    def send(self, opcode: int, payload: bytes = b"") -> None:
        header = struct.pack(">H", len(payload) + 4) + struct.pack("<I", opcode)
        with self._send_lock:
            if self._tx_cipher is not None:
                header = self._tx_cipher.apply(header)
            self.sock.sendall(header + payload)

    # ---- handshake -----------------------------------------------------
    def _handshake(self) -> None:
        opcode, body = self._read_packet()
        if opcode != SMSG_AUTH_CHALLENGE:
            raise WorldError(f"expected SMSG_AUTH_CHALLENGE, got {opcode:#x}")
        b = _Buf(body)
        b.u32()
        server_seed = b.u32()

        client_seed = struct.unpack("<I", os.urandom(4))[0]
        # WorldSocket::HandleAuthSession: SHA1(account, u32 0, client seed, server seed, K)
        client_proof = hashlib.sha1(
            self.username.encode() + bytes(4) + struct.pack("<I", client_seed)
            + struct.pack("<I", server_seed) + self.session_key).digest()

        # Empty addon list, zlib-compressed: u32 count=0, u32 time=0.
        addon_raw = struct.pack("<II", 0, 0)
        addons = struct.pack("<I", len(addon_raw)) + zlib.compress(addon_raw)
        payload = (
            struct.pack("<II", BUILD, 0)
            + self.username.encode() + b"\0"
            + struct.pack("<I", 0)
            + struct.pack("<I", client_seed)
            + struct.pack("<III", 0, 0, self.realm_id)
            + struct.pack("<Q", 0)
            + bytes(client_proof)
            + addons
        )
        self.send(CMSG_AUTH_SESSION, payload)
        # Everything after AUTH_SESSION has encrypted headers, both directions.
        self._rx_cipher = _Rc4(hmac.new(_SERVER_ENCRYPTION_KEY, self.session_key, hashlib.sha1).digest())
        self._tx_cipher = _Rc4(hmac.new(_SERVER_DECRYPTION_KEY, self.session_key, hashlib.sha1).digest())

        opcode, body = self._read_packet()
        if opcode == SMSG_WARDEN_DATA:
            raise WorldError("server started Warden — the OS tag must be OSX")
        if opcode != SMSG_AUTH_RESPONSE:
            raise WorldError(f"expected SMSG_AUTH_RESPONSE, got {opcode:#x}")
        result = body[0]
        if result != AUTH_OK:
            raise WorldError(f"world auth failed: result={result:#x}")

    # ---- reader / dispatcher --------------------------------------------
    def _read_loop(self) -> None:
        try:
            while not self._stop.is_set():
                opcode, body = self._read_packet()
                try:
                    self._dispatch(opcode, body)
                except Exception as e:  # never let one bad parse kill the session
                    self._event("parse_error", opcode=hex(opcode), error=repr(e))
        except Exception as e:
            if not self._stop.is_set():
                self._event("disconnected", error=repr(e))
                self.in_world = False

    def _event(self, kind: str, **data) -> None:
        with self._lock:
            self.events.append(Event(time.time(), kind, data))

    def _dispatch(self, opcode: int, body: bytes) -> None:
        if opcode == SMSG_TIME_SYNC_REQ:
            counter = struct.unpack("<I", body[:4])[0]
            ms = int((time.monotonic() - self._t0) * 1000) & 0xFFFFFFFF
            self.send(CMSG_TIME_SYNC_RESP, struct.pack("<II", counter, ms))
        elif opcode in (SMSG_MESSAGECHAT, SMSG_GM_MESSAGECHAT):
            self._on_chat(body, gm=opcode == SMSG_GM_MESSAGECHAT)
        elif opcode == SMSG_NAME_QUERY_RESPONSE:
            b = _Buf(body)
            guid = b.packed_guid()
            if b.u8() == 0:
                name = b.cstr()
                self.names[guid] = name
                with self._lock:
                    for c in self.chat:
                        if c.sender_guid == guid and not c.sender_name:
                            c.sender_name = name
        elif opcode == MSG_MOVE_TELEPORT_ACK:
            b = _Buf(body)
            guid = b.packed_guid()
            counter = b.u32()
            ms = int((time.monotonic() - self._t0) * 1000) & 0xFFFFFFFF
            self.send(MSG_MOVE_TELEPORT_ACK, _packed_guid(guid) + struct.pack("<II", counter, ms))
            self._event("teleport_near")
        elif opcode == SMSG_NEW_WORLD:
            b = _Buf(body)
            mapid = b.u32()
            self.send(MSG_MOVE_WORLDPORT_ACK)
            self._event("teleport_far", map=mapid)
        elif opcode == SMSG_GROUP_INVITE:
            b = _Buf(body)
            status = b.u8()
            self._event("group_invite", inviter=b.cstr(), status=status)
        elif opcode == SMSG_GROUP_LIST:
            self._event("group_list", size=len(body))
        elif opcode in (SMSG_GROUP_DESTROYED, SMSG_GROUP_UNINVITE):
            self._event("group_left", opcode=hex(opcode))
        elif opcode == SMSG_PARTY_COMMAND_RESULT:
            b = _Buf(body)
            op = b.u32()
            name = b.cstr()
            self._event("party_result", op=op, name=name, result=b.u32())
        elif opcode == SMSG_LOGIN_VERIFY_WORLD:
            self.in_world = True
            self._event("in_world", map=struct.unpack("<I", body[:4])[0])
        elif opcode == SMSG_LOGOUT_COMPLETE:
            self._event("logout_complete")
        elif opcode == SMSG_WARDEN_DATA:
            self._event("warden", size=len(body))
        else:
            self._inbox.put((opcode, body))

    def _on_chat(self, body: bytes, gm: bool) -> None:
        b = _Buf(body)
        ctype = b.u8()
        b.i32()          # language
        sender = b.u64()
        b.u32()          # flags
        sender_name = ""
        if ctype in _NAMED_SENDER:
            b.u32()
            sender_name = b.cstr()
            receiver = b.u64()
            # BuildChatPacket adds the receiver's name unless it is a player (high 0x0000) or a pet (0xF140).
            if receiver and (receiver >> 48) not in (0x0000, 0xF140):
                b.u32()
                b.cstr()
        elif ctype == _WHISPER_FOREIGN:
            b.u32()
            sender_name = b.cstr()
            b.u64()
        elif ctype in _BG_SYSTEM:
            receiver = b.u64()
            if receiver and (receiver >> 48) != 0x0000:
                b.u32()
                b.cstr()
        elif ctype in _ACHIEVEMENT:
            b.u64()
        else:
            if gm:
                b.u32()
                sender_name = b.cstr()
            if ctype == _CHANNEL:
                b.cstr()
            b.u64()      # receiver
        b.u32()          # message length incl. NUL
        text = b.cstr()
        if not sender_name:
            sender_name = self.names.get(sender, "")
            if not sender_name and sender:
                self.name_query(sender)   # the reply backfills this event (see NAME_QUERY_RESPONSE)
        with self._lock:
            self.chat.append(ChatEvent(time.time(), ctype, sender, sender_name, text, gm))

    def _keepalive_loop(self) -> None:
        seq = 0
        # 32 s, never faster: WorldSocket::HandlePing kicks a session for
        # "over-speed pings" (< ~27 s apart) — measured 2026-09-25 at 25 s.
        while not self._stop.wait(32):
            try:
                self.send(CMSG_KEEP_ALIVE)
                seq += 1
                self.send(CMSG_PING, struct.pack("<II", seq, 50))
            except Exception:
                return

    # ---- waiting helpers ---------------------------------------------------
    def expect(self, opcode: int, timeout: float = 15.0) -> bytes:
        deadline = time.monotonic() + timeout
        stash = []
        try:
            while True:
                left = deadline - time.monotonic()
                if left <= 0:
                    raise WorldError(f"timed out waiting for opcode {opcode:#x}")
                op, body = self._inbox.get(timeout=left)
                if op == opcode:
                    return body
                stash.append((op, body))
        except queue.Empty:
            raise WorldError(f"timed out waiting for opcode {opcode:#x}")
        finally:
            for item in stash:
                self._inbox.put(item)

    def wait_until(self, pred, timeout: float, poll: float = 0.25):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            v = pred()
            if v:
                return v
            time.sleep(poll)
        return None

    # ---- characters --------------------------------------------------------
    def characters(self) -> list[Character]:
        self.send(CMSG_CHAR_ENUM)
        b = _Buf(self.expect(SMSG_CHAR_ENUM))
        out = []
        for _ in range(b.u8()):
            guid = b.u64()
            name = b.cstr()
            race, cls = b.u8(), b.u8()
            b.skip(6)                       # gender, skin, face, hair style/colour, facial hair
            level = b.u8()
            zone, mapid = b.u32(), b.u32()
            b.skip(12 + 4 + 4 + 4 + 1)      # x,y,z, guild, char flags, customize flags, first login
            b.skip(12)                      # pet display, level, family
            b.skip(23 * 9)                  # equipment: display u32, inv type u8, enchant u32
            out.append(Character(guid, name, race, cls, level, mapid, zone))
            self.names[guid] = name
        return out

    def create_character(self, name: str, race: int = 2, cls: int = 1, gender: int = 0) -> None:
        payload = name.encode() + b"\0" + bytes([race, cls, gender, 0, 0, 0, 0, 0, 0])
        self.send(CMSG_CHAR_CREATE, payload)
        code = self.expect(SMSG_CHAR_CREATE)[0]
        if code != CHAR_CREATE_SUCCESS:
            raise WorldError(f"character create failed: code={code:#x}")

    def enter_world(self, char: Character, timeout: float = 30.0) -> None:
        self.me = char
        self.send(CMSG_PLAYER_LOGIN, struct.pack("<Q", char.guid))
        if not self.wait_until(lambda: self.in_world, timeout):
            raise WorldError("no SMSG_LOGIN_VERIFY_WORLD")
        self.send(CMSG_READY_FOR_ACCOUNT_DATA_TIMES)
        self.send(CMSG_SET_ACTIVE_MOVER, struct.pack("<Q", char.guid))

    # ---- actions -----------------------------------------------------------
    def name_query(self, guid: int) -> None:
        self.send(CMSG_NAME_QUERY, struct.pack("<Q", guid))

    def whisper(self, target: str, text: str) -> None:
        self.send(CMSG_MESSAGECHAT, struct.pack("<II", CHAT_WHISPER, LANG_ORCISH)
                  + target.encode() + b"\0" + text.encode() + b"\0")

    def party(self, text: str) -> None:
        self.send(CMSG_MESSAGECHAT, struct.pack("<II", CHAT_PARTY, LANG_ORCISH) + text.encode() + b"\0")

    def say(self, text: str) -> None:
        self.send(CMSG_MESSAGECHAT, struct.pack("<II", CHAT_SAY, LANG_ORCISH) + text.encode() + b"\0")

    def invite(self, name: str) -> None:
        self.send(CMSG_GROUP_INVITE, name.encode() + b"\0" + struct.pack("<I", 0))

    def accept_invite(self) -> None:
        self.send(CMSG_GROUP_ACCEPT, struct.pack("<I", 0))

    def decline_invite(self) -> None:
        self.send(CMSG_GROUP_DECLINE)

    def leave_group(self) -> None:
        self.send(CMSG_GROUP_DISBAND)

    def select(self, guid: int) -> None:
        self.send(CMSG_SET_SELECTION, struct.pack("<Q", guid))

    def logout(self, timeout: float = 30.0) -> None:
        self.send(CMSG_LOGOUT_REQUEST)
        self.wait_until(lambda: any(e.kind == "logout_complete" for e in self.events), timeout)

    def close(self) -> None:
        self._stop.set()
        try:
            self.sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        self.sock.close()

    # ---- assertions helpers --------------------------------------------------
    def chat_since(self, t: float) -> list[ChatEvent]:
        with self._lock:
            return [c for c in self.chat if c.ts >= t]

    def events_since(self, t: float, kind: str | None = None) -> list[Event]:
        with self._lock:
            return [e for e in self.events if e.ts >= t and (kind is None or e.kind == kind)]
