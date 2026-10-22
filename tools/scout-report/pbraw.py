"""Decode a base64 protobuf blob schema-less and print it as an indented tree (field numbers only).

usage: python3 -I pbraw.py <file.b64>
"""
import sys,base64,struct
def varint(b,i):
    r=s=0
    while True:
        c=b[i];i+=1;r|=(c&0x7f)<<s;s+=7
        if c<0x80:return r,i
def parse(b):
    i=0;out=[]
    while i<len(b):
        k,i=varint(b,i);f,t=k>>3,k&7
        if f==0:raise ValueError
        if t==0:v,i=varint(b,i)
        elif t==1:v=struct.unpack('<d',b[i:i+8])[0];i+=8
        elif t==5:v=struct.unpack('<f',b[i:i+4])[0];i+=4
        elif t==2:
            n,i=varint(b,i);v=b[i:i+n];i+=n
            if i>len(b):raise ValueError
        else:raise ValueError
        out.append((f,t,v))
    return out
def show(b,d=0):
    for f,t,v in parse(b):
        p='  '*d
        if t==2:
            try:
                sub=parse(v)
                if sub: print(f"{p}{f} {{");show(v,d+1);print(p+"}");continue
            except Exception:pass
            try:print(f"{p}{f}: {v.decode()!r}")
            except Exception:print(f"{p}{f}: bytes {v.hex()}")
        elif t==5:print(f"{p}{f}: {v:g}f")
        else:print(f"{p}{f}: {v}")
if __name__=="__main__": show(base64.b64decode(open(sys.argv[1]).read().strip()))
