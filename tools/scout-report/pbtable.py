"""Print every leaf of a scout report as: path | raw bytes (tag + value, hex) | decoded value | meaning.

usage: python3 -I pbtable.py <report.b64>   (output: Markdown table)
Meanings are the inferences documented in docs/scouting.mdx.
"""
import base64
import re
import struct
import sys

HEROES = {50006: "Murphy", 40012: "Violet", 40010: "Mason", 40020: "Monica", 40019: "Sarah"}
RES = {1: "métal", 14: "pain", 2: "bois"}
STAT = {
    50014: "PV du héros", 50015: "attaque du héros", 50017: "défense du héros",
    50006: "PV des soldats", 50007: "attaque des soldats", 50009: "défense des soldats",
    50019: "PV des soldats (2e jeu)", 50016: "attaque des soldats (2e jeu)", 50018: "défense des soldats (2e jeu)",
    50005: "équipement : PV soldats", 50008: "équipement : attaque soldats", 50010: "équipement : défense soldats",
    50020: "équipement : PV soldats (copie)", 50021: "équipement : attaque soldats (copie)", 50022: "équipement : défense soldats (copie)",
    50011: "taux de critique", 50012: "dégâts critiques (multiplicateur)", 50013: "inconnu (clé 152018)",
    50039: "tanks : PV soldats", 50040: "tanks : PV soldats (2e)", 50041: "tanks : attaque soldats",
    50042: "tanks : attaque soldats (2e)", 50043: "tanks : défense soldats", 50044: "tanks : défense soldats (2e)",
    50052: "avions : PV héros", 50053: "avions : PV héros", 50054: "avions : attaque héros", 50055: "avions : attaque héros",
    50056: "avions : défense héros", 50057: "avions : défense héros",
    50060: "drone : PV héros", 50061: "drone : attaque héros", 50062: "drone : défense héros",
    50063: "PV héros (variante)", 50064: "PV héros (variante)",
    50071: "capacité de troupes (variante)", 50072: "capacité de troupes : bonus tanks",
    50073: "capacité de troupes (variante)", 50074: "capacité de troupes (variante)",
    50079: "capacité de troupes totale", 80008: "capacité de troupes de base",
    50081: "drone : PV", 50082: "drone : attaque", 50083: "drone : défense",
    50084: "part du drone (10 %) : PV", 50085: "part du drone (10 %) : attaque", 50086: "part du drone (10 %) : défense",
    50091: "drone : PV (type 8, inconnu)", 50092: "drone : attaque (type 8, inconnu)", 50093: "drone : défense (type 8, inconnu)",
    50098: "drone : PV (copie)", 50099: "drone : attaque (copie)", 50100: "drone : défense (copie)",
    50137: "capacité de l'hôpital", 50155: "effet de compétence",
    50030: "ratio héros (clé 152028)", 50031: "ratio héros (clé 152029)", 50032: "ratio (clé 152024)", 50033: "ratio (clé 152025)",
    51001: "PV soldats (variante)", 51051: "attaque soldats (variante)", 51101: "défense soldats (variante)",
    53000: "PV héros tiré du drone", 53010: "attaque héros tirée du drone", 53020: "défense héros tirée du drone",
    71013: "bonus de récolte", 71016: "bonus de récolte",
    75050: "% PV soldats", 75053: "% PV soldats tanks", 75055: "% PV soldats avions",
    75066: "% PV soldats tanks (2e)", 75068: "% PV soldats avions (2e)",
    75150: "% attaque soldats", 75153: "% attaque soldats tanks", 75155: "% attaque soldats avions",
    75166: "% attaque soldats tanks (2e)", 75168: "% attaque soldats avions (2e)",
    75250: "% défense soldats", 75253: "% défense soldats tanks", 75255: "% défense soldats avions",
    75266: "% défense soldats tanks (2e)", 75268: "% défense soldats avions (2e)",
    75949: "% dégâts de compétence", 75950: "% dégâts de compétence",
    75953: "% dégâts contre les zombies", 75957: "% soldats (clé 221008)", 75959: "% soldats (clé 221009)",
    75965: "% soldats tanks (clé 221008)", 75967: "% héros avions (clé 221009)",
    76053: "ratio inconnu (clé 152043)", 76201: "capacité de troupes tanks", 76203: "capacité de troupes avions",
}


def varint(b, i):
    r = s = 0
    while True:
        c = b[i]
        i += 1
        r |= (c & 0x7F) << s
        s += 7
        if c < 0x80:
            return r, i


def parse(b):
    """[(field, wiretype, value, raw_bytes)]; raises on anything that isn't a clean message."""
    i, out = 0, []
    while i < len(b):
        start = i
        k, i = varint(b, i)
        f, t = k >> 3, k & 7
        if f == 0:
            raise ValueError
        if t == 0:
            v, i = varint(b, i)
        elif t == 5:
            v = struct.unpack("<f", b[i:i + 4])[0]
            i += 4
        elif t == 1:
            v = struct.unpack("<d", b[i:i + 8])[0]
            i += 8
        elif t == 2:
            n, i = varint(b, i)
            v = b[i:i + n]
            i += n
            if i > len(b):
                raise ValueError
        else:
            raise ValueError
        out.append((f, t, v, b[start:i]))
    return out


def leaves(b, path=""):
    try:
        items = parse(b)
    except Exception:
        items = None
    if not items:
        return
    seen = {}
    for f, t, v, raw in items:
        seen[f] = seen.get(f, -1) + 1
        p = f"{path}.{f}[{seen[f]}]"
        if t == 2:
            try:
                sub = parse(v)
            except Exception:
                sub = None
            if sub:
                yield from leaves(v, p)
                continue
            try:
                yield p, raw, repr(v.decode())
            except UnicodeDecodeError:
                yield p, raw, "octets " + v.hex()
        elif t == 5:
            yield p, raw, f"{v:g} (float)"
        else:
            yield p, raw, str(v)


def meaning(p, val, ctx):
    q = re.sub(r"\[0\]", "", p)
    m = re.match(r"^\.4\.1\.1\[(\d)\]\.2\[(\d)\]\.(.*)$", p.replace("[0]", "[0]"))
    hero = re.match(r"^\.4\[0\]\.1\[0\]\.1\[(\d)\]\.2\[(\d)\]\.(.+)$", p)
    if hero:
        sq, h, rest = hero.groups()
        name = ctx["heroes"].get((sq, h), f"héros {int(h) + 1}")
        who = f"escouade {int(sq) + 1}, {name}"
        if rest == "1[0].1[0]":
            return f"{who} : id du héros ({HEROES.get(int(val), '?')})"
        if rest == "2[0].1[0]":
            return f"{who} : niveau"
        if rest == "4[0].1[0]":
            return f"{who} : rang ou étoiles (brut, sens incertain)"
        if rest == "5[0].1[0]":
            return f"{who} : place dans l'escouade"
        if rest in ("6[0].1[0]", "7[0].1[0]"):
            return f"{who} : = stat 50014, PV du héros (probable)"
        s = re.match(r"^11\[(\d+)\]\.(\d)\[0\]$", rest)
        if s:
            sid = ctx["stat"].get((sq, h, s.group(1)))
            if s.group(2) == "1":
                return f"{who} : id de stat → {STAT.get(int(val), 'inconnue')}"
            return f"{who} : valeur de la stat {sid} ({STAT.get(sid, 'inconnue')})"
        return f"{who} : champ vide, inconnu"
    st = re.match(r"^\.(15\[0\]\.4|16\[0\]\.10)\[(\d+)\]\.(\d)\[0\]$", p)
    if st:
        sid = ctx["stat"].get((st.group(1), st.group(2)))
        where = "drone" if st.group(1).startswith("15") else "bonus de ville"
        if where == "drone" and sid in (50006, 50007, 50009):
            sid_label = {50006: "drone : PV (copie)", 50007: "drone : attaque (copie)", 50009: "drone : défense (copie)"}[sid]
            return sid_label if st.group(3) == "2" else f"drone : id de stat {val} → {sid_label}"
        if st.group(3) == "1":
            return f"{where} : id de stat → {STAT.get(int(val), 'inconnue')}"
        return f"{where} : valeur de la stat {sid} ({STAT.get(sid, 'inconnue')})"
    res = re.match(r"^\.3\[0\]\.1\[(\d)\]\.(\d)\[0\]\.1\[0\]$", p)
    if res:
        r = ctx["res"].get(res.group(1))
        return {"2": f"quantité pillable ({RES.get(r, '?')})", "3": f"id de ressource ({RES.get(int(val), '?')})",
                "4": f"quantité pillable, copie ({RES.get(r, '?')})"}[res.group(2)]
    sold = re.match(r"^\.4\[0\]\.1\[0\]\.5\[(\d+)\]\.(\d)\[0\](.*)$", p)
    if sold:
        if sold.group(2) == "2":
            return f"id de soldat (niveau {int(val) - 3004})"
        return "nombre de soldats" if sold.group(3) else "nombre de soldats : vide = 0"
    fixed = {
        ".1[0].2[0]": "nom de la cible", ".1[0].3[0]": "avatar", ".1[0].4[0]": "vide (sigle d'alliance ? aucune)",
        ".1[0].5[0].1[0].1[0]": "serveur de la position", ".1[0].5[0].2[0].1[0]": "position X",
        ".1[0].5[0].3[0].1[0]": "position Y", ".1[0].5[0].4[0]": "vide, inconnu",
        ".1[0].6[0]": "uid de la cible", ".1[0].9[0]": "niveau du QG (probable)", ".1[0].10[0]": "serveur",
        ".2[0].1[0].1[0]": "durabilité de la ville (probable)", ".2[0].2[0].1[0]": "durabilité max (probable)",
        ".2[0].3[0].1[0]": "indicateur, inconnu", ".3[0].2[0].1[0]": "indicateur, inconnu",
        ".4[0].1[0].2[0].1[0]": "indicateur, inconnu", ".4[0].1[0].4[0]": "inconnu (puissance des troupes ?)",
        ".4[0].1[0].7[0].1[0]": "id de stat → capacité de l'hôpital",
        ".4[0].1[0].7[0].2[0]": "capacité de l'hôpital (probable)",
        ".4[0].2[0].2[0].1[0]": "indicateur, inconnu", ".4[0].3[0].1[0].1[0]": "indicateur, inconnu",
        ".4[0].3[0].2[0]": "vide, inconnu", ".5[0].2[0].1[0]": "indicateur, inconnu", ".6[0]": "vide, inconnu",
        ".7[0].3[0].1[0]": "inconnu", ".8[0]": "vide, inconnu", ".15[0].1[0]": "drone : id ou niveau (inconnu)",
        ".15[0].2[0]": "drone : indicateur, inconnu", ".16[0].12[0]": "vide, inconnu",
        ".17[0]": "indicateur, inconnu", ".101[0]": "indicateur ou version, inconnu",
    }
    if p in fixed:
        return fixed[p]
    if re.match(r"^\.4\[0\]\.1\[0\]\.1\[(\d)\]\.(5|6)\[0\]$", p):
        return "escouade : numéro"
    if re.match(r"^\.4\[0\]\.1\[0\]\.1\[\d\]\.8\[0\]$", p):
        return "escouade : vide, inconnu"
    if re.match(r"^\.4\[0\]\.1\[0\]\.3\[\d+\]$", p):
        return "emplacement vide (renfort ?)"
    if re.match(r"^\.16\[0\]\.5\[\d\]\.1\[0\]$", p):
        return "liste d'ids, inconnue"
    if re.match(r"^\.16\[0\]\.15\[0\]\.3\[\d\]\.1\[0\]$", p):
        return "emplacement n°"
    if re.match(r"^\.16\[0\]\.15\[0\]\.3\[\d\]\.3\[0\]$", p):
        return "emplacement débloqué (1)"
    return "inconnu"


def main():
    b = base64.b64decode(open(sys.argv[1]).read().strip())
    rows = list(leaves(b))
    ctx = {"heroes": {}, "stat": {}, "res": {}}
    for p, raw, val in rows:  # first pass: what each repeated entry is
        m = re.match(r"^\.4\[0\]\.1\[0\]\.1\[(\d)\]\.2\[(\d)\]\.1\[0\]\.1\[0\]$", p)
        if m:
            ctx["heroes"][m.groups()] = HEROES.get(int(val), "?")
        m = re.match(r"^\.4\[0\]\.1\[0\]\.1\[(\d)\]\.2\[(\d)\]\.11\[(\d+)\]\.1\[0\]$", p)
        if m:
            ctx["stat"][m.groups()] = int(val)
        m = re.match(r"^\.(15\[0\]\.4|16\[0\]\.10)\[(\d+)\]\.1\[0\]$", p)
        if m:
            ctx["stat"][m.groups()] = int(val)
        m = re.match(r"^\.3\[0\]\.1\[(\d)\]\.3\[0\]\.1\[0\]$", p)
        if m:
            ctx["res"][m.group(1)] = int(val)
    print("| Chemin | Octets encodés (hex) | Valeur décodée | Correspondance |")
    print("|---|---|---|---|")
    for p, raw, val in rows:
        print(f"| `{p}` | `{raw.hex(' ')}` | {val} | {meaning(p, val, ctx)} |")


if __name__ == "__main__":
    main()
