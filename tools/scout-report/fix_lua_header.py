"""Rewrite the game's non-standard Lua 5.3 chunk header into a stock one.

The game sets the header's format byte to 1 and drops the 1-byte instruction-size
field (see docs/methodology.mdx). This restores format=0 and inserts sizeof(Instruction)=4,
so a stock 32-bit lua5.3 (size_t=4, like the game's build) can load the chunk.

usage: python3 -I fix_lua_header.py <in_dir> <out_dir>
"""
import os
import sys

src, dst = sys.argv[1:3]
os.makedirs(dst, exist_ok=True)
ok = bad = 0
for n in os.listdir(src):
    b = open(os.path.join(src, n), "rb").read()
    # \x1bLua 0x53 format LUAC_DATA(6) int size_t [instruction size missing] integer number
    if b[:5] == b"\x1bLuaS" and b[5] == 1 and b[6:12] == b"\x19\x93\r\n\x1a\n":
        b = b[:5] + b"\x00" + b[6:14] + b"\x04" + b[14:]
        ok += 1
    else:
        bad += 1
    open(os.path.join(dst, n), "wb").write(b)
print("fixed", ok, "unrecognized", bad)
