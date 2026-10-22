"""Print every leaf of a base64 protobuf blob as one path per line, e.g. .4[0].1[0].2[3].1[0] 40020.

usage: python3 -I pbpaths.py <file.b64> <dir containing pbraw.py>
"""
import sys,base64
sys.path.insert(0,sys.argv[2]); from pbraw import parse
def walk(b,path):
    try: items=parse(b)
    except Exception: items=None
    if not items:
        try: print(path, repr(b.decode()))
        except Exception: print(path,'bytes',b.hex())
        return
    idx={}
    for f,t,v in items:
        idx[f]=idx.get(f,-1)+1
        p=f"{path}.{f}[{idx[f]}]"
        if t==2: walk(v,p)
        elif t==5: print(p,f"{v:g}f")
        else: print(p,v)
walk(base64.b64decode(open(sys.argv[1]).read().strip()),'')
